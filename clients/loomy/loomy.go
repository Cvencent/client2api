// Package loomy implements the "loomy" client: an OpenAI-shaped façade over
// 讯飞 Loomy, iFlytek's Loomy desktop AI agent, whose backend is the
// OpenAI-compatible endpoint at https://loomyad.xunfei.cn/api/v1.
//
// The wire protocol is genuinely OpenAI-shaped -- /chat/completions with a
// standard SSE stream -- so this module's job is not translation but honesty
// about the credential.  Loomy authenticates with a user *session* that the
// vendor issues at SMS or WeChat login, lives for 14 days, and CANNOT be
// renewed: there is no refresh endpoint anywhere in the vendor's API, and the
// reference implementation says so explicitly.  When a session dies the only
// remedy is for the operator to log in again and import the new token, so that
// is exactly what this module says, in Status, in the panel account record and
// in every error it returns.  Nothing here ever mints a credential.
//
// Two smaller protocol details are load-bearing and are implemented exactly as
// the reference documents them:
//
//   - /chat/completions accepts `Authorization: Bearer <session>` while
//     /models and /points/* accept only a lowercase `token` header.  Sending
//     the wrong one yields HTTP 200 with {"code":"100002"}, so the chat request
//     sends BOTH headers.
//   - The credit multiplier is part of the model's *name* string ("... · x3.0"),
//     not a field, so the catalogue normalises it into the display name rather
//     than dropping it -- hiding it would hide the price.
//
// PROVENANCE: a clean-room Go implementation written from the reference
// project's TypeScript sources (src/loomy.ts, src/loomy-product.ts,
// src/loomy-sign.ts, src/loomy-auth.ts, src/loomy-adapter.ts, src/loomy-oauth.ts,
// src/loomy-credits.ts).  No reference code, comment or identifier was copied;
// the signing routine is Go's crypto/hmac rather than a transliteration of the
// reference's JavaScript.  See README.md for the provenance statement and for
// what could not be verified without a live account.
package loomy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
	"client2api/internal/smscap"
)

func init() { core.Register("loomy", New) }

const (
	// modelsRetryInterval throttles background catalogue refreshes, so a
	// failing upstream cannot be hammered once per /v1/models call.
	modelsRetryInterval = 30 * time.Second
)

// Client implements core.Client for loomy, plus the optional interfaces listed
// in README.md.
type Client struct {
	deps  core.Deps
	cfg   config
	up    *upstream
	store *store

	// Paths inside Deps.DataDir; empty when no DataDir was supplied, in which
	// case nothing is persisted anywhere and the pool lives only in memory.
	accountsPath string
	statePath    string

	modelsMu        sync.Mutex
	models          []core.Model
	modelsAt        time.Time
	modelsAttemptAt time.Time
	modelsLoading   bool

	stateMu   sync.Mutex
	lastErr   string
	lastErrAt time.Time

	// board caches the vendor's onboarding completion flags for one account.
	// See onboarding.go.
	board onboardingBoard

	// smsRot hands out the next province in the one-time-SMS rotation
	// (sms.go).  autos is the in-flight auto-login job table (autologin.go);
	// it is created lazily by putAutoJob.
	smsRot smscap.Rotator
	autoMu sync.Mutex
	autos  map[string]*autoJob
}

// New builds the client.  It never fails for a missing or malformed credential:
// both are runtime conditions reported by Status(), not construction failures.
func New(deps core.Deps) (core.Client, error) {
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		// A typo in one timeout must never take the module off the panel.
		deps.Log("loomy: invalid config, using defaults: %v", err)
		cfg = config{}
	}
	applyEnv(&cfg)

	if deps.DataDir != "" {
		if err := core.EnsureDir(deps.DataDir); err != nil {
			deps.Log("loomy: creating data dir: %v", err)
		}
	}

	c := &Client{deps: deps, cfg: cfg}
	c.setPaths()

	st, err := loadStore(c.accountsPath, c.statePath)
	if err != nil {
		// A corrupt store is reported and the module carries on empty: the
		// operator can re-import, which is better than refusing to start.
		deps.Log("loomy: %v", err)
	}
	c.store = st
	c.store.applyConfig(configuredAccounts(cfg))

	c.up = newUpstream(cfg, deps.Proxy, deps.Logf)
	if fp := cfg.tlsProfile(); fp != fingerprint.ProfileNone {
		fhc, ferr := fingerprint.New(fingerprint.Options{
			Profile:  fp,
			Protocol: cfg.tlsProtocol(),
			Proxy:    deps.Proxy,
			Logf:     deps.Logf,
		})
		if ferr != nil {
			deps.Log("loomy: tls fingerprint %q ignored: %v", string(fp), ferr)
		} else {
			c.up.json = fhc
			c.up.sse = fhc
		}
	}
	return c, nil
}

// setPaths resolves the module's files inside Deps.DataDir.  With no DataDir it
// leaves them empty, so the module can never write outside its own directory.
func (c *Client) setPaths() {
	if c.deps.DataDir == "" {
		return
	}
	c.accountsPath = filepath.Join(c.deps.DataDir, accountsFile)
	c.statePath = filepath.Join(c.deps.DataDir, stateFileName)
}

// Name is both the registry key and the routing prefix ("loomy/<model>").
func (c *Client) Name() string { return "loomy" }

// newUpstream builds the HTTP layer.  The two clients differ in exactly one
// way: the streaming one has no overall timeout, because a stream lives as long
// as it lives and is bounded by the module's own idle and chat deadlines.
func newUpstream(cfg config, proxy string, logf func(string, ...any)) *upstream {
	return &upstream{
		cfg:  cfg,
		json: &http.Client{Transport: transportFor(proxy)},
		sse:  &http.Client{Transport: transportFor(proxy)},
		logf: logf,
	}
}

func transportFor(proxy string) *http.Transport {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return tr
}

// ---------------------------------------------------------------------------
// models
// ---------------------------------------------------------------------------

// Models returns the catalogue.  It is deliberately non-blocking: the vendor's
// list is refreshed in the background, so this answers from the cache when it is
// fresh and from the built-in catalogue otherwise.  The registry calls Models on
// every module to resolve a bare model name, so it has to stay cheap.
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
	return fallbackCatalogue(), nil
}

// RefreshModels implements core.ModelRefresher.  Unlike Models it is
// synchronous, because the panel's "re-fetch from upstream" button is an
// explicit request that has to report success or failure.
//
// A failed refresh must never empty the catalogue: every error path returns the
// last good list -- or the built-in catalogue when nothing has ever been
// fetched -- alongside the error, so one flaky refresh cannot blank the model
// picker.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	previous, _ := c.cachedModels()
	fallback := previous
	if len(fallback) == 0 {
		fallback = fallbackCatalogue()
	}

	rctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()

	models, err := c.fetchModels(rctx)
	if err != nil {
		c.noteFailure(err)
		// "Not configured" is a state, not a failure: the caller has to be able
		// to recognise it, so it is passed through unwrapped.  Anything else is
		// given the context of the operation that failed.
		if errors.Is(err, core.ErrNotConfigured) {
			return fallback, core.ErrNotConfigured
		}
		return fallback, fmt.Errorf("loomy: refresh models: %s", redactErr(err))
	}
	c.storeModels(models)
	return models, nil
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

	core.GoSafe("loomy model refresh", func(msg string) { c.logf("%s", msg) }, func() {
		defer func() {
			c.modelsMu.Lock()
			c.modelsLoading = false
			c.modelsMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.modelsTimeout())
		defer cancel()

		models, err := c.fetchModels(ctx)
		if err != nil {
			c.noteFailure(err)
			c.logf("loomy: model list: %v", redactErr(err))
			return
		}
		c.storeModels(models)
	})
}

// fetchModels asks the vendor for the live catalogue, trying accounts in the
// order the pool wants them tried.
func (c *Client) fetchModels(ctx context.Context) ([]core.Model, error) {
	candidates := c.store.candidates(time.Now().UTC())
	if len(candidates) == 0 {
		return nil, core.ErrNotConfigured
	}

	attempts := c.cfg.maxAttempts()
	var lastErr error
	for i, acc := range candidates {
		if i >= attempts {
			break
		}
		models, err := c.up.modelList(ctx, acc.AccessToken)
		if err == nil {
			if len(models) == 0 {
				lastErr = errors.New("loomy: the vendor returned an empty model list")
				continue
			}
			c.store.reset(acc.ID, time.Now().UTC())
			return coreModels(models, false), nil
		}
		lastErr = err
		// The same rule the Chat loop below spells out: a caller that walked
		// away is not evidence about the credential, and retrying the remaining
		// candidates would just burn them on a request nobody is waiting for.
		//
		// This only ever fires for a real caller leaving.  Models() runs
		// fetchModels on a WithTimeout child of the caller's context, so a
		// caller cancellation propagates in as context.Canceled while our own
		// models deadline arrives as DeadlineExceeded and still parks the
		// account; refreshModelsAsync passes a background context, which has no
		// ancestor that could be cancelled at all.
		if callerGone(err) {
			break
		}
		c.penalise(acc.ID, err, time.Now().UTC())
	}
	if lastErr == nil {
		lastErr = core.ErrNotConfigured
	}
	return nil, lastErr
}

func (c *Client) storeModels(models []core.Model) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	c.models = models
	c.modelsAt = time.Now()
	// This call just did the work, so do not let the background refresher
	// immediately repeat it.
	c.modelsAttemptAt = time.Now()
}

func (c *Client) cachedModels() ([]core.Model, time.Time) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	return c.models, c.modelsAt
}

// modelIDs is the bare list Status reports.
func (c *Client) modelIDs() []string {
	models, _ := c.cachedModels()
	if len(models) == 0 {
		models = fallbackCatalogue()
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

// ModelMaxOutputTokens implements core.ModelLimitsProvider.
//
// The vendor does publish a budget -- every chat row of GET /api/v1/models
// carries `max_output_tokens` (measured live: deepseek-v4-flash-0731 384000,
// GLM-5.3-Flash 131072) -- and this module used to discard it.  The interface
// forbids fetching, so this answers from the live catalogue cache only: a cold
// cache declines rather than triggering a network call, and the built-in
// fallback table is deliberately NOT consulted, because it carries no
// vendor-published number and inventing one is exactly the silent truncation
// the interface exists to prevent.
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	id := strings.TrimSpace(model)
	if id == "" {
		return 0, false
	}
	list, _ := c.cachedModels()
	if len(list) == 0 {
		return 0, false
	}
	return core.OutputLimitFor(list, id)
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

// Chat opens one streamed completion.  The model arrives with the "loomy/"
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
	if c.store.count() == 0 {
		return nil, core.ErrNotConfigured
	}

	// The caller may name a model by its raw id or by the display name the
	// catalogue advertises.  Either way the vendor wants its own id.  The
	// request is copied rather than mutated, so a caller that reuses the struct
	// still sees the model it asked for.
	local := *req
	local.Model = resolveModelID(c.cachedOrFallback(), req.Model)

	body, err := buildBody(&local)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	candidates := c.store.candidates(now)
	if len(candidates) == 0 {
		return nil, c.unavailableError(now)
	}

	attempts := c.cfg.maxAttempts()
	var lastErr error
	// busy records that every candidate we reached was at its per-account
	// in-flight ceiling.
	busy := false
	for i, acc := range candidates {
		if i >= attempts {
			break
		}
		// Take the account's slot before the network call, not after: the
		// ceiling has to bound what reaches the vendor.  A full account is
		// skipped so the next candidate is tried.
		if err := req.AcquireAccountSlot(acc.ID); err != nil {
			busy = true
			continue
		}
		stream, err := c.up.openChatStream(ctx, acc.AccessToken, body)
		if err == nil {
			c.store.reset(acc.ID, time.Now().UTC())
			// Tell the gateway which credential actually served this request.
			// The slot is nil in most unit tests, and NoteServedBy is a no-op
			// then, so this is safe to call unconditionally.
			core.NoteServedBy(req, acc.ID)
			return stream, nil
		}
		lastErr = err
		// A cancelled caller is not evidence about the credential.  The
		// operator pressed Stop, or the browser went away; the request failed
		// for a reason that says nothing about this account.  Parking it would
		// take a healthy credential out of the pool, and with max_attempts > 1
		// a single cancellation would burn the remaining candidates too.
		//
		// Our own first-byte deadline does not land here: openChatStream
		// reports that as its own error text and leaves ctx.Err() nil.  So a
		// non-nil ctx.Err() means the caller really did walk away.
		if ctx.Err() != nil {
			break
		}

		c.penalise(acc.ID, err, time.Now().UTC())
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

// cachedOrFallback is the catalogue the request is resolved against.
func (c *Client) cachedOrFallback() []core.Model {
	models, _ := c.cachedModels()
	if len(models) == 0 {
		models = fallbackCatalogue()
	}
	return models
}

// resolveModelID turns whatever the caller named into the vendor's own model id.
//
// The catalogue advertises both spellings: Model.ID is the raw id, and
// Extra["display_name"] is the human name -- which includes the credit
// multiplier, because Loomy encodes the multiplier inside the name.  A caller
// that copied the display name out of the panel must still reach the model, so
// both spellings are accepted, as is the display name with the multiplier
// stripped.  A name that matches nothing is returned VERBATIM: the vendor is the
// authority on its own model ids, and inventing a mapping here would only hide
// the real error behind a wrong one.
func resolveModelID(models []core.Model, want string) string {
	want = strings.TrimSpace(want)
	if want == "" {
		return want
	}
	for _, m := range models {
		if strings.EqualFold(m.ID, want) {
			return m.ID
		}
	}
	for _, m := range models {
		if name, ok := m.Extra["display_name"].(string); ok && strings.EqualFold(name, want) {
			return m.ID
		}
	}
	for _, m := range models {
		name, ok := m.Extra["display_name"].(string)
		if ok && strings.EqualFold(modelNameWithoutRate(name), want) {
			return m.ID
		}
	}
	return want
}

// unavailableError explains why nothing could be tried.  It distinguishes a
// session that is genuinely gone from an account that is merely resting, because
// the operator's next action is completely different in the two cases.
func (c *Client) unavailableError(now time.Time) error {
	for _, a := range c.store.snapshot() {
		if a.dead || a.expired(now) {
			return errSessionExpired(a.ID)
		}
	}
	return errors.New("loomy: every account is parked or cooling down; retry shortly")
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

// Status is deliberately cheap: no network calls and one mutex-guarded
// snapshot.  The panel calls it every ten seconds.
func (c *Client) Status(ctx context.Context) core.Status {
	now := time.Now().UTC()
	st := core.Status{
		Name:      "loomy",
		UpdatedAt: time.Now(),
		Models:    c.modelIDs(),
	}

	accounts := c.store.snapshot()
	records := make([]core.AccountStatus, 0, len(accounts))
	usable, gone := 0, 0
	for i := range accounts {
		records = append(records, accountStatus(&accounts[i], now))
		if accounts[i].selectable(now) {
			usable++
		}
		if accounts[i].dead || accounts[i].expired(now) {
			gone++
		}
	}
	st.Accounts = records

	switch {
	case len(accounts) == 0:
		st.Ready = false
		st.Detail = "no credential: import a Loomy session token, or set " +
			"clients.loomy.access_token / CLIENT2API_LOOMY_ACCESS_TOKEN"
	case usable == 0 && gone > 0:
		st.Ready = false
		st.Detail = "the session has expired and Loomy has no refresh endpoint; log in " +
			"again (SMS or WeChat) and import the new token"
	case usable == 0:
		st.Ready = false
		st.Detail = "every account is parked or cooling down"
	default:
		st.Ready = true
		st.Detail = fmt.Sprintf("%d of %d accounts usable; models: %s",
			usable, len(accounts), strings.Join(st.Models, ", "))
	}

	if msg, at := c.lastFailure(); msg != "" && !st.Ready {
		st.Detail += "; last error " + msg + " (" + humanAge(time.Since(at)) + " ago)"
	}
	return st
}

// noteFailure remembers the last upstream failure for Status.  Only the message
// is kept, and it has already been redacted by the caller's path.
func (c *Client) noteFailure(err error) {
	if err == nil {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.lastErr = redactErr(err)
	c.lastErrAt = time.Now()
}

func (c *Client) lastFailure() (string, time.Time) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.lastErr, c.lastErrAt
}
