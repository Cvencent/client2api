// Package gemini implements the Gemini (Google Cloud Code Assist) upstream --
// the free line the Antigravity IDE rides on.  See gemini.go for the constants
// and the three byte-level constraints the vendor enforces.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

func init() {
	core.Register(clientName, New)
}

// Client is the Gemini module.
type Client struct {
	deps core.Deps
	cfg  Config
	now  func() time.Time

	pool     *pool
	sig      *sigStore
	affinity *core.Affinity

	// defaultClient is used when the harness did not hand us one.
	defaultClient *http.Client

	// proxyMu guards proxyClient, which is installed when clients.gemini.proxy
	// (or a proxy supplied with the login start) must override the shared
	// process-wide HTTP client for every Gemini request.
	proxyMu     sync.RWMutex
	proxyClient *http.Client

	// probedProject caches the loadCodeAssist answer per account so a request
	// does not re-probe on every turn when the vendor returned an empty
	// project (which is the free tier's normal answer).
	projectMu     sync.Mutex
	probedProject map[string]string

	// sessionGen holds the per-base-session generation counter used by the
	// 1M-accumulation self-heal.
	sessionMu  sync.Mutex
	sessionGen map[string]int

	// modelsMu guards the live catalogue: the served rows plus the routing
	// table that maps an advertised id onto the upstream admission key.
	// modelsTried throttles background retries and modelsLoading is the
	// single-flight latch; see models.go.
	modelsMu      sync.Mutex
	models        []core.Model
	routes        map[string]modelRoute
	modelsAt      time.Time
	modelsTried   time.Time
	modelsLoading bool

	loginMu sync.Mutex
	logins  map[string]*loginSession
	errMu   sync.Mutex
	lastErr string
}

// New builds the module.  It never fails: a rejected config is remembered and
// reported through Status and Chat.
func New(deps core.Deps) (core.Client, error) {
	c := &Client{
		deps:          deps,
		now:           time.Now,
		pool:          newPool(),
		probedProject: map[string]string{},
		sessionGen:    map[string]int{},
		routes:        map[string]modelRoute{},
		logins:        map[string]*loginSession{},
		defaultClient: &http.Client{Transport: newTransport(deps.Proxy)},
	}
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		deps.Log("gemini: config rejected, continuing with defaults: %v", err)
		cfg = Config{}
	}
	c.cfg = cfg.normalize()
	c.setProxy(c.cfg.Proxy)
	c.sig = newSigStore(sigStorePath(deps.DataDir))
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()

	recs := c.loadAccounts()
	if len(recs) > 0 {
		deps.Log("gemini: loaded %d account(s)", len(recs))
	}
	return c, nil
}

func (c *Client) ensure() {
	if c.now == nil {
		c.now = time.Now
	}
	if c.pool == nil {
		c.pool = newPool()
	}
	if c.probedProject == nil {
		c.probedProject = map[string]string{}
	}
	if c.sessionGen == nil {
		c.sessionGen = map[string]int{}
	}
	c.modelsMu.Lock()
	if c.routes == nil {
		c.routes = map[string]modelRoute{}
	}
	c.modelsMu.Unlock()
	if c.affinity == nil {
		c.affinity = core.NewAffinity(0)
		c.affinity.StartGC()
	}
	c.loginMu.Lock()
	if c.logins == nil {
		c.logins = map[string]*loginSession{}
	}
	c.loginMu.Unlock()
}

func (c *Client) Name() string { return clientName }

func (c *Client) httpClient() *http.Client {
	c.proxyMu.RLock()
	proxyClient := c.proxyClient
	c.proxyMu.RUnlock()
	if proxyClient != nil {
		return proxyClient
	}
	if c.deps.HTTPClient != nil {
		return c.deps.HTTPClient
	}
	if c.defaultClient != nil {
		return c.defaultClient
	}
	return &http.Client{Transport: newTransport(c.deps.Proxy)}
}

// setProxy installs or clears the module-specific egress.  An empty proxy
// deliberately falls back to the shared process client, so clearing the
// setting in the panel restores the gateway-wide route.
func (c *Client) setProxy(proxy string) {
	proxy = strings.TrimSpace(proxy)
	var client *http.Client
	if proxy != "" {
		client = &http.Client{Transport: newTransport(proxy)}
	}
	c.proxyMu.Lock()
	c.proxyClient = client
	c.proxyMu.Unlock()
}

func (c *Client) credentialsPath() string { return clientPath(c.deps.DataDir, credentialsFile) }

// loadAccounts builds the initial pool from the credential store plus any
// credential the config file supplied.
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

// upsertAccount stores or replaces a credential and persists the store.
func (c *Client) upsertAccount(rec accountRecord) error {
	c.ensure()
	if err := c.saveAccount(rec, true); err != nil {
		return err
	}
	c.pool.upsert(rec)
	return nil
}

// saveAccount writes one record into the store file, replacing an existing row.
func (c *Client) saveAccount(rec accountRecord, persist bool) error {
	if !persist {
		return nil
	}
	recs := c.pool.snapshot()
	found := false
	for i := range recs {
		if recs[i].ID == rec.ID {
			recs[i] = rec
			found = true
			break
		}
	}
	if !found {
		recs = append(recs, rec)
	}
	return saveCredentials(c.credentialsPath(), recs)
}

// persistPool writes the current pool back to disk.
func (c *Client) persistPool() {
	c.ensure()
	if err := saveCredentials(c.credentialsPath(), c.pool.snapshot()); err != nil {
		c.noteError("saving credentials: " + err.Error())
	}
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

// maxRotate bounds the account rotation, counting the first attempt.
const maxRotate = 3

// Chat opens a completion.
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
	resolved, ok := c.resolveModel(req.Model, req)
	if !ok {
		return nil, fmt.Errorf("%w: model %q is not in this provider's catalogue; pick one from %s/ or refresh the list in the panel", core.ErrUnsupported, req.Model, clientName)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())
	stream, err := c.openChat(ctx, cancel, req, resolved)
	if err != nil {
		cancel()
		return nil, err
	}
	return stream, nil
}

// openChat picks an account and opens the upstream stream, rotating on
// failure.
func (c *Client) openChat(ctx context.Context, cancel context.CancelFunc, req *core.ChatRequest, resolved resolvedModel) (core.Stream, error) {
	var lastErr error
	for attempt := 0; attempt < maxRotate; attempt++ {
		if err := ctx.Err(); err != nil {
			break
		}
		acct, err := c.acquireAccount(req)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		if err := req.AcquireAccountSlot(acct.ID); err != nil {
			c.pool.release(acct.ID)
			lastErr = err
			continue
		}
		c.bindServedConversation(req, acct.ID)
		if err := c.ensureFresh(ctx, acct); err != nil {
			c.pool.release(acct.ID)
			// The token endpoint refused the refresh token itself: nothing a
			// retry can fix, so park the credential as 账号异常 rather than
			// cooling it down and re-selecting it on the next loop.
			if isRefreshRejected(err) {
				c.pool.markRejected(acct.ID, "the vendor rejected the refresh token; sign in again")
				c.persistPool()
			}
			lastErr = c.classify(ctx, acct, err)
			continue
		}
		core.NoteServedBy(req, acct.ID)
		stream, err := c.streamOnce(ctx, cancel, acct, req, resolved)
		if err != nil {
			c.pool.release(acct.ID)
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			lastErr = c.classify(ctx, acct, err)
			continue
		}
		// The vendor served this request, so any earlier "this credential was
		// rejected" verdict is stale.
		c.pool.noteServed(acct.ID)
		return stream, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("%w: no Gemini account could serve the request", core.ErrBusy)
}

// streamOnce performs one upstream request attempt.
func (c *Client) streamOnce(ctx context.Context, cancel context.CancelFunc, acct *accountRecord, req *core.ChatRequest, resolved resolvedModel) (core.Stream, error) {
	project := c.projectFor(ctx, acct)
	lane := c.laneFor(req)
	generation := c.generationFor(project, req, lane)
	envelope, err := buildEnvelopeFor(project, resolved.upstream, resolved.tier, req, c.sig, lane, generation)
	if err != nil {
		return nil, err
	}
	payload, err := marshalRequest(envelope)
	if err != nil {
		return nil, err
	}

	endpoints := []string{endpointDaily, endpointSandbox}
	var lastErr error
	for i, endpoint := range endpoints {
		resp, err := c.postStream(ctx, endpoint, acct.AccessToken, payload)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return newSSEStream(ctx, cancel, resp.Body, c.sig), nil
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()

		// A signature rejection: retry once without signatures.
		if resp.StatusCode == http.StatusBadRequest && isSignatureError(string(body)) {
			envelopeNoSig, berr := buildEnvelopeFor(project, resolved.upstream, resolved.tier, req, nil, lane, generation)
			if berr == nil {
				if payload2, merr := marshalRequest(envelopeNoSig); merr == nil {
					if resp2, perr := c.postStream(ctx, endpoint, acct.AccessToken, payload2); perr == nil && resp2.StatusCode == http.StatusOK {
						return newSSEStream(ctx, cancel, resp2.Body, c.sig), nil
					} else if resp2 != nil {
						resp2.Body.Close()
					}
				}
			}
		}

		// Server-side session accumulation: bump the generation and retry once.
		if resp.StatusCode == http.StatusBadRequest && isSessionOverflow(string(body)) {
			if c.bumpSession(project, req, c.laneFor(req)) {
				lastErr = fmt.Errorf("gemini: %s", describeBody(resp.StatusCode, body))
				break
			}
		}

		lastErr = c.classifyHTTP(acct, resp.StatusCode, body)
		// Rotate endpoints only on a switchable 4xx; a 5xx from the daily
		// endpoint is worth trying on the sandbox too, so continue.
		if i == 0 && shouldTryNextEndpoint(resp.StatusCode, string(body)) {
			continue
		}
		break
	}
	if lastErr == nil {
		lastErr = errors.New("gemini: the upstream returned no response")
	}
	return nil, lastErr
}

// postStream sends the streaming request with the identity headers (and NO
// Accept header, matching the vendor's own client).
func (c *Client) postStream(ctx context.Context, endpoint, token string, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+streamPath, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	headers := identityHeadersFor(token, false)
	applyExtraHeaders(headers, c.cfg.ExtraHeaders)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.httpClient().Do(req)
}

// ensureFresh renews an account's token when it is near expiry.
func (c *Client) ensureFresh(ctx context.Context, acct *accountRecord) error {
	if !isExpired(acct, c.now()) {
		return nil
	}
	if acct.RefreshToken == "" {
		return fmt.Errorf("credential is expired and has no refresh_token")
	}
	grant, err := c.refreshGrant(ctx, acct.RefreshToken)
	if err != nil {
		return err
	}
	expiry := expiryFrom(grant.ExpiresIn)
	if c.pool.updateTokens(acct.ID, grant.AccessToken, grant.RefreshToken, expiry, grant.TokenType) {
		c.persistPool()
	}
	acct.AccessToken = grant.AccessToken
	acct.RefreshToken = grant.RefreshToken
	acct.Expiry = expiry
	return nil
}

// isExpired reports whether a credential is within the 60-second refresh
// margin of its expiry.  An unparseable expiry counts as "refresh now".
func isExpired(acct *accountRecord, now time.Time) bool {
	if acct == nil || acct.Expiry == "" {
		return false
	}
	expires, err := time.Parse(time.RFC3339, acct.Expiry)
	if err != nil {
		return true
	}
	return expires.Add(-60 * time.Second).Before(now)
}

// ---------------------------------------------------------------------------
// project resolution
// ---------------------------------------------------------------------------

// projectFor returns the project id to put in the envelope: the configured
// override, else the account's cached value, else a loadCodeAssist probe, else
// the default.  Only a real probe answer is persisted.
func (c *Client) projectFor(ctx context.Context, acct *accountRecord) string {
	if c.cfg.Project != "" {
		return c.cfg.Project
	}
	if acct.Project != "" {
		return acct.Project
	}
	c.projectMu.Lock()
	cached := c.probedProject[acct.ID]
	c.projectMu.Unlock()
	if cached != "" {
		return cached
	}
	project, tier := c.probeProject(ctx, acct)
	if project != "" {
		if c.pool.setProject(acct.ID, project) {
			c.persistPool()
		}
	} else {
		project = defaultProject
	}
	if tier != "" && c.pool.setTier(acct.ID, tier) {
		c.persistPool()
	}
	c.projectMu.Lock()
	c.probedProject[acct.ID] = project
	c.projectMu.Unlock()
	return project
}

// probeProject calls loadCodeAssist.  A failure returns empty values so the
// caller falls back rather than failing the request.
func (c *Client) probeProject(ctx context.Context, acct *accountRecord) (project, tier string) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.quotaTimeout())
	defer cancel()
	status, body, err := c.doJSON(ctx, endpointSandbox, loadCodeAssistPath, acct.AccessToken, rawJSON(loadCodeAssistBody), false)
	if err != nil || status < 200 || status >= 300 {
		return "", ""
	}
	var resp struct {
		CloudAICodeProject string `json:"cloudaicompanionProject"`
		PaidTier           *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"paidTier"`
		CurrentTier *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"currentTier"`
	}
	if jsonUnmarshal(body, &resp) != nil {
		return "", ""
	}
	t := resp.PaidTier
	if t == nil {
		t = resp.CurrentTier
	}
	if t != nil {
		tier = tierLabel(t.ID, t.Name)
	}
	return strings.TrimSpace(resp.CloudAICodeProject), tier
}

// tierLabel maps a tier id/name to the panel's short label.
func tierLabel(id, name string) string {
	combined := strings.ToLower(id + " " + name)
	switch {
	case strings.Contains(combined, "ultra"):
		return "Ultra"
	case strings.Contains(combined, "pro"):
		return "Pro"
	case strings.Contains(combined, "free"):
		return "Free"
	}
	return firstNonEmpty(name, id)
}

// ---------------------------------------------------------------------------
// lane + session generation
// ---------------------------------------------------------------------------

// laneFor returns the session-derivation lane.  Inference uses "infer".
func (c *Client) laneFor(req *core.ChatRequest) string { return "infer" }

func (c *Client) sessionKey(project string, req *core.ChatRequest) string {
	tier := effortToTier(requestEffort(req))
	_ = tier
	return project
}

// bumpSession increments the generation for a base session, returning true when
// it changed (so the caller retries once).
func (c *Client) bumpSession(project string, req *core.ChatRequest, lane string) bool {
	key := c.sessionKey(project, req) + "\x00" + lane
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	gen := c.sessionGen[key]
	if gen >= 1 {
		return false
	}
	c.sessionGen[key] = gen + 1
	return true
}

// generationFor returns the current generation for a base session.  It is read
// inside buildEnvelope via the caller.
func (c *Client) generationFor(project string, req *core.ChatRequest, lane string) int {
	key := c.sessionKey(project, req) + "\x00" + lane
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	return c.sessionGen[key]
}

// ---------------------------------------------------------------------------
// error classification
// ---------------------------------------------------------------------------

// classifyHTTP maps an upstream status onto a core error and updates the pool.
//
// A 401 is singled out from the other auth-ish statuses because it is the only
// one that says the credential itself is not accepted.  Parking it for one
// cooldown and re-selecting it is the loop that leaves the panel showing 冷却中
// for a token only a re-login can repair, so the account is disabled and the row
// becomes 账号异常 with the re-login control.  403 is left on the cooling path on
// purpose: Gemini answers it for a project that lacks a permission or a quota,
// neither of which a re-login fixes.
func (c *Client) classifyHTTP(acct *accountRecord, status int, body []byte) error {
	msg := describeBody(status, body)
	switch {
	case status == http.StatusUnauthorized:
		c.pool.markRejected(acct.ID, "the vendor rejected this credential (HTTP 401): "+msg)
		c.persistPool()
		return fmt.Errorf("%w: %s", core.ErrNotConfigured, msg)
	case status == http.StatusForbidden:
		c.pool.noteError(acct.ID, msg, c.now(), 1, c.cfg.cooldown())
		c.persistPool()
		return fmt.Errorf("%w: %s", core.ErrNotConfigured, msg)
	case status == http.StatusTooManyRequests:
		c.pool.setCooldown(acct.ID, c.now().Add(c.cfg.cooldown()), msg)
		c.persistPool()
		return fmt.Errorf("%w: %s", core.ErrBusy, msg)
	case status == http.StatusBadRequest && isSessionOverflow(string(body)):
		return errors.New(msg)
	case status >= 500:
		return errors.New(msg)
	default:
		return errors.New(msg)
	}
}

func (c *Client) classify(ctx context.Context, acct *accountRecord, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if err == nil {
		return nil
	}
	c.noteError(err.Error())
	return err
}

// shouldTryNextEndpoint reports whether a failed status warrants the other
// endpoint.  5xx and switchable 4xx do; a 401 does not (the credential is bad
// on both).
func shouldTryNextEndpoint(status int, body string) bool {
	if status >= 500 {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}
	lower := strings.ToLower(body)
	for _, w := range []string{"quota", "permission", "unsupported", "project"} {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// isSignatureError is deliberately broad: the vendor's wording varies, and a
// false positive only costs one extra request without signatures.
func isSignatureError(text string) bool {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "signature") {
		return true
	}
	return strings.Contains(lower, "thought") && strings.Contains(lower, "invalid")
}

// isSessionOverflow reports the server-side 1M accumulation refusal.
func isSessionOverflow(text string) bool {
	return strings.Contains(strings.ToLower(text), "exceeds the maximum number of tokens")
}

// ---------------------------------------------------------------------------
// error notes
// ---------------------------------------------------------------------------

func (c *Client) noteError(msg string) {
	msg = truncate(core.Redact(msg), 300)
	c.errMu.Lock()
	changed := msg != c.lastErr
	c.lastErr = msg
	c.errMu.Unlock()
	if changed {
		c.deps.Log("gemini: %s", msg)
	}
}

func (c *Client) lastErrorNote() string {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.lastErr
}

// ---------------------------------------------------------------------------
// Status / Health / PoolStats
// ---------------------------------------------------------------------------

func (c *Client) Status(ctx context.Context) core.Status {
	c.ensure()
	c.warmCatalogue()
	now := c.now()
	st := core.Status{
		Name:      clientName,
		UpdatedAt: now,
		Accounts:  c.pool.statuses(now),
		Models:    c.modelIDs(),
	}
	if len(st.Accounts) == 0 {
		st.Detail = "no Gemini account yet; sign in from the panel"
	} else {
		st.Ready = true
		st.Detail = c.pool.summary(now, c.cfg.maxInFlight())
	}
	if msg := c.lastErrorNote(); msg != "" {
		st.Detail += "; last error: " + msg
	}
	return st
}

// Health implements core.HealthProvider.
func (c *Client) Health() core.Health {
	c.ensure()
	now := c.now()
	h := core.Health{}
	for _, a := range c.pool.snapshot() {
		h.Total++
		switch {
		case !a.Enabled || a.AccessToken == "":
			h.Disabled++
		case coolingDown(&a, now):
			h.Cooling++
		default:
			h.Ready++
		}
	}
	h.Servable = h.Ready > 0
	if !h.Servable {
		if h.Total == 0 {
			h.Note = "no Gemini account; sign in from the panel"
		} else if h.Disabled == h.Total {
			h.Note = "every Gemini account is disabled"
		} else {
			h.Note = "no Gemini account has a free slot right now"
		}
	}
	return h
}

// PoolStats implements core.PoolStatsReporter.
func (c *Client) PoolStats() core.PoolStats {
	c.ensure()
	inFlight, full := c.pool.stats(c.cfg.maxInFlight())
	sticky := 0
	if c.affinity != nil {
		sticky = c.affinity.Count()
	}
	return core.PoolStats{InFlight: inFlight, InFlightFull: full, StickySessions: sticky}
}

// ReviveAccount implements core.Reviver: it clears this process's penalties for
// one account so the pool retries it immediately.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	c.ensure()
	if !c.pool.clearCooldown(id) {
		return fmt.Errorf("account %s not found", id)
	}
	c.persistPool()
	return nil
}

var (
	_ core.Client            = (*Client)(nil)
	_ core.ModelRefresher    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
	_ core.HealthProvider    = (*Client)(nil)
	_ core.Reviver           = (*Client)(nil)
	_ core.LoginProvider     = (*Client)(nil)
)
