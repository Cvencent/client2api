// Package tabbit exposes the "tabbit" client: a supervisor and proxy for the
// locally installed tabbit2api sidecar (a browser-driving HTTP service), not a
// port of it.  The upstream project (hwttop5/tabbit2api) is GPL-3.0-only, so
// this module contains no code derived from it: it locates, optionally starts
// and forwards to the sidecar's HTTP surface, and degrades honestly when the
// sidecar is absent.
package tabbit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

func init() { core.Register("tabbit", New) }

// Client implements core.Client for the tabbit sidecar.
type Client struct {
	deps   core.Deps
	cfg    Config
	cfgErr error

	getenv func(string) string

	mu         sync.Mutex
	health     healthState
	catalog    []core.Model
	catalogAt  time.Time
	discovered string
	lastErr    string

	// acctMu guards the panel's endpoint store (accounts.go).  It is separate
	// from mu so a slow health probe never blocks the account list.
	acctMu sync.Mutex
	acct   *endpointFile

	// Browser-login hand-offs (login.go).  Lazily created so a Client
	// assembled by a test without New keeps working.
	loginMu sync.Mutex
	logins  map[string]*loginSession

	// webMu guards webChecks, the last verdict each web-token account got from
	// web.tabbit.com (web.go).  It is separate from mu so a slow vendor call
	// never blocks the status path.
	webMu     sync.Mutex
	webChecks map[string]webVerdict

	// affinity pins a conversation to the account that already served it.  See
	// affinity.go.  It is created by New, and every method on it is nil-safe, so
	// a Client assembled by a test without New keeps working.
	affinity *core.Affinity

	// inFlight is the number of chat streams currently open across both
	// transports.  The success paths feed it through core.TrackStream, so
	// PoolStats reports real concurrency rather than a second bookkeeping.
	inFlight atomic.Int64

	// vendorClient, when non-nil, replaces the shared HTTP client with one
	// whose TLS handshake imitates a browser.  It is set by New when the config
	// names a tls_profile; nil keeps the stock handshake.
	vendorClient *http.Client

	sup *supervisor
}

// New never fails: an unusable configuration or a missing sidecar is reported
// through Status and Chat (core.ErrNotConfigured) instead of taking the whole
// process down at start-up.
func New(deps core.Deps) (core.Client, error) {
	c := &Client{
		deps:   deps,
		getenv: os.Getenv,
		sup:    newSupervisor(filepath.Join(deps.DataDir, "sidecar.log")),
		acct:   loadEndpoints(deps.DataDir),
	}
	// A zero TTL selects core's documented default window; the sweep runs for
	// the life of the process and is started here so the first binding is
	// already guarded against an operator who never reloads the config.
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()
	cfg, err := parseConfig(deps.Config)
	c.cfg = cfg.normalize()
	if err != nil {
		c.cfgErr = err
		deps.Log("config rejected, continuing with defaults: %v", err)
	}
	// A named profile replaces the shared client with one whose TLS
	// handshake imitates that browser.  The web transport uses a real
	// session cookie against the vendor's own site, so a Go-shaped hello is
	// a detectable tell.  Empty keeps the stock client.
	if fp := c.cfg.tlsProfile(); fp != fingerprint.ProfileNone {
		fhc, ferr := fingerprint.New(fingerprint.Options{
			Profile:  fp,
			Protocol: c.cfg.tlsProtocol(),
			Proxy:    c.deps.Proxy,
			Logf:     c.deps.Logf,
		})
		if ferr != nil {
			deps.Log("tabbit: tls fingerprint %q ignored: %v", string(fp), ferr)
		} else {
			c.vendorClient = fhc
		}
	}
	return c, nil
}

func (c *Client) Name() string { return "tabbit" }

// Models returns the sidecar's model ids with the "tabbit/" prefix stripped
// (the gateway re-adds it).  It never fails: when the sidecar cannot be
// reached it returns the built-in fallback catalogue so routing and the panel
// stay useful.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.cfgErr != nil {
		return fallbackModels(c.cfg), nil
	}
	if c.webRoute() {
		models, err := c.webModels(ctx)
		if err != nil {
			c.noteError(err.Error())
			if last := c.lastCatalog(); len(last) > 0 {
				return last, nil
			}
			return fallbackModels(c.cfg), nil
		}
		return models, nil
	}
	if models, ok := c.cachedCatalog(); ok {
		return models, nil
	}
	loc := c.locate()
	if strings.TrimSpace(loc.baseURL) == "" {
		return fallbackModels(c.cfg), nil
	}
	models, err := c.fetchModels(ctx, loc)
	if err != nil {
		c.noteError(err.Error())
		return fallbackModels(c.cfg), nil
	}
	c.storeCatalog(models, time.Now())
	c.setDiscovered(loc.baseURL)
	return models, nil
}

// RefreshModels implements core.ModelRefresher: it re-reads the sidecar's
// catalogue, deliberately bypassing the models cache TTL that Models() honours.
// The panel calls it behind its "re-fetch from upstream" button, so it must do
// the round trip even when the cache is still warm.
//
// A failed refresh must never empty the catalogue.  On every error path the
// last good list is returned alongside the error -- or the built-in fallback
// when nothing has ever been fetched -- so one flaky refresh cannot blank the
// model picker.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.cfgErr != nil {
		return fallbackModels(c.cfg), fmt.Errorf("tabbit: refresh models: %w", c.cfgErr)
	}

	fallback := c.lastCatalog()
	if len(fallback) == 0 {
		fallback = fallbackModels(c.cfg)
	}

	if c.webRoute() {
		wa, err := c.resolveWebAuth()
		if err != nil {
			return fallback, fmt.Errorf("tabbit: refresh models: %w", err)
		}
		models, err := c.webFetchModels(ctx, wa)
		if err != nil {
			c.noteError(err.Error())
			return fallback, fmt.Errorf("tabbit: refresh models: %s", scrubSecret(err.Error(), wa.token))
		}
		c.storeCatalog(models, time.Now())
		c.setDiscovered(wa.base)
		return models, nil
	}

	loc := c.locate()
	if strings.TrimSpace(loc.baseURL) == "" {
		return fallback, fmt.Errorf("tabbit: refresh models: no sidecar endpoint is known")
	}
	models, err := c.fetchModels(ctx, loc)
	if err != nil {
		c.noteError(err.Error())
		return fallback, fmt.Errorf("tabbit: refresh models: %s", scrubSecret(err.Error(), loc.apiKey))
	}
	c.storeCatalog(models, time.Now())
	c.setDiscovered(loc.baseURL)
	return models, nil
}

// scrubSecret renders an upstream failure without ever echoing the API key we
// sent.  core.Redact catches credential-shaped text; the key itself is
// replaced verbatim as well, because a bare key carries no label for
// core.Redact to key on.
func scrubSecret(msg, secret string) string {
	msg = core.Redact(msg)
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, core.MaskSecret(secret))
	}
	return msg
}

// Chat forwards one completion request to the sidecar and re-frames its SSE
// answer into core events.  It is safe for concurrent use.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.cfgErr != nil {
		return nil, fmt.Errorf("%w: %v", core.ErrNotConfigured, c.cfgErr)
	}
	// The stickiness key and the model are derived once, before the route is
	// chosen.  The key must not change between the web and the sidecar path: it
	// names the conversation, not the transport, so a conversation that falls
	// back from a dead web session to the sidecar re-binds the SAME key rather
	// than starting a second, parallel history.  An empty key means the request
	// is not conversation-scoped, which turns the whole feature off for it.
	convKey := conversationKey(req)
	model := ""
	if req != nil {
		model = strings.TrimSpace(stripModelPrefix(req.Model, c.cfg.ModelPrefix))
	}
	if c.webRoute() {
		return c.openWebStream(ctx, req)
	}
	// Validate and serialise first: an unsupported request shape must not
	// depend on the sidecar being up.
	body, err := buildChatBody(c.cfg, req)
	if err != nil {
		return nil, err
	}
	loc := c.locate()
	// pickSidecar only answers when the panel store is what chose the endpoint;
	// when the config, the environment or the state file named it, there is no
	// account to pin and locate()'s answer stands unchanged.
	if ep, ok := c.pickSidecar(loc, convKey, model); ok {
		loc = c.locationFor(ep, loc)
	}
	loc, err = c.resolve(ctx, loc)
	if err != nil {
		c.noteError(err.Error())
		return nil, err
	}
	// Name the credential for the gateway's usage ledger and console row.  On
	// the sidecar transport the endpoint *is* the account: it is the id the
	// account table reports, and resolve() may have started or moved it.
	// Take the endpoint's in-flight slot before the call.  The endpoint is
	// the account on this transport, so a saturated endpoint is backpressure.
	if err := req.AcquireAccountSlot(loc.baseURL); err != nil {
		c.noteError(err.Error())
		return nil, err
	}
	core.NoteServedBy(req, loc.baseURL)
	stream, err := c.openStream(ctx, loc, body)
	if err != nil {
		c.noteError(err.Error())
		return nil, err
	}
	c.inFlight.Add(1)
	return core.TrackStream(stream, func() { c.inFlight.Add(-1) }), nil
}

// Status reports honestly what this module knows right now.  It never starts
// the sidecar and never blocks for more than the status budget (0.9s by
// default), because the panel refreshes it every 10 seconds.
func (c *Client) Status(ctx context.Context) core.Status {
	if ctx == nil {
		ctx = context.Background()
	}
	st := core.Status{Name: "tabbit", UpdatedAt: time.Now(), Models: c.statusModels()}

	if c.cfgErr != nil {
		st.Detail = "invalid config: " + truncate(c.cfgErr.Error(), 160)
		st.Accounts = []core.AccountStatus{{
			ID: "config", Label: "clients.tabbit config", Enabled: true,
			State: "invalid", Note: "the clients.tabbit object could not be parsed",
		}}
		return st
	}

	if c.webRoute() {
		return c.webStatus(ctx, st)
	}

	loc := c.locate()
	if strings.TrimSpace(loc.baseURL) == "" {
		st.Detail = "sidecar not configured: no base_url (install tabbit2api or set clients.tabbit.base_url)"
		st.Accounts = []core.AccountStatus{{
			ID: "sidecar", Label: "tabbit2api sidecar", Enabled: true,
			State: "unknown", Note: "not configured",
		}}
		return st
	}

	h := c.probe(ctx, loc, c.cfg.Timeouts.statusBudget())
	c.storeHealth(h)

	acct := core.AccountStatus{ID: loc.baseURL, Label: "tabbit2api sidecar", Enabled: true}
	switch {
	case h.ok:
		acct.State = "ready"
		acct.Note = "local api key from " + loc.keySource
		if h.version != "" {
			acct.Note = "version " + h.version + ", " + acct.Note
		}
		st.Ready = true
		st.Detail = fmt.Sprintf("sidecar %s ok (%s, %s%s)",
			loc.baseURL, versionLabel(h), modelCountLabel(h), hostSuffix(h, c.cfg.WebHost))
	case h.authRejected():
		acct.State = "invalid"
		acct.Note = "the sidecar rejected the local api key"
		st.Detail = fmt.Sprintf("sidecar %s rejected our local api key (%s)", loc.baseURL, h.err)
	default:
		acct.State = "unknown"
		acct.Note = truncate(h.err, 160)
		if loc.explicit() {
			st.Detail = fmt.Sprintf("sidecar %s not reachable: %s", loc.baseURL, h.err)
		} else {
			st.Detail = fmt.Sprintf("no sidecar configured and nothing listening on %s (%s)", loc.baseURL, h.err)
		}
		if c.lastErr != "" && c.lastErr != h.err {
			st.Detail += "; last error: " + truncate(c.lastErr, 120)
		}
	}
	st.Accounts = []core.AccountStatus{acct}
	return st
}

// ---------------------------------------------------------------------------
// Location and supervision
// ---------------------------------------------------------------------------

// resolve decides which endpoint to talk to.  When the user configured a base
// URL we use it as-is (starting the process only if "manage": true and the
// health check fails); otherwise we look for a sidecar already listening on a
// default port.
func (c *Client) resolve(ctx context.Context, loc location) (location, error) {
	if loc.explicit() {
		if c.cfg.Manage && !c.probe(ctx, loc, c.cfg.Timeouts.healthBudget()).ok {
			if err := c.startSidecar(ctx, loc); err != nil {
				return loc, fmt.Errorf("%w: %v", core.ErrNotConfigured, err)
			}
		}
		return loc, nil
	}

	for _, cand := range defaultCandidates {
		try := loc
		try.baseURL = cand
		try.source = "probe"
		if st := c.probe(ctx, try, c.cfg.Timeouts.healthBudget()); st.ok {
			c.storeHealth(st)
			c.setDiscovered(cand)
			return try, nil
		}
	}

	if c.cfg.Manage {
		try := loc
		try.baseURL = firstCandidate()
		try.source = "managed"
		if err := c.startSidecar(ctx, try); err != nil {
			return loc, fmt.Errorf("%w: %v", core.ErrNotConfigured, err)
		}
		return try, nil
	}

	return loc, fmt.Errorf("%w: no tabbit2api sidecar is listening on %s and none is configured (set clients.tabbit.base_url, or start the sidecar yourself)",
		core.ErrNotConfigured, strings.Join(defaultCandidates, ", "))
}

// startSidecar launches the sidecar process (only reachable with "manage": true)
// and waits for its health endpoint to answer.
func (c *Client) startSidecar(ctx context.Context, loc location) error {
	if c.sup == nil {
		c.sup = newSupervisor(filepath.Join(c.deps.DataDir, "sidecar.log"))
	}
	if c.sup.running() {
		return c.waitHealthy(ctx, loc)
	}
	if strings.TrimSpace(loc.command) == "" {
		return fmt.Errorf("no sidecar command found (set clients.tabbit.command, or install tabbit2api and put it on PATH)")
	}
	if err := c.sup.start(loc); err != nil {
		return err
	}
	c.deps.Log("started sidecar %s (log: %s)", filepath.Base(loc.command), c.sup.logPath)
	if err := c.waitHealthy(ctx, loc); err != nil {
		return err
	}
	c.writeState(loc)
	return nil
}

func (c *Client) waitHealthy(ctx context.Context, loc location) error {
	deadline := time.Now().Add(c.cfg.Timeouts.startBudget())
	var last healthState
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = c.probe(ctx, loc, 2*time.Second)
		if last.ok {
			c.storeHealth(last)
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("sidecar %s did not answer %s within %s: %s",
				loc.baseURL, c.cfg.HealthPath, c.cfg.Timeouts.startBudget(), last.err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// writeState remembers how the sidecar was started so a later run can reuse it.
// The local api key is deliberately not persisted: it comes from the config,
// the environment or the built-in default.
func (c *Client) writeState(loc location) {
	if strings.TrimSpace(c.deps.DataDir) == "" {
		return
	}
	if err := core.EnsureDir(c.deps.DataDir); err != nil {
		c.deps.Log("cannot create data dir %s: %v", c.deps.DataDir, err)
		return
	}
	path := filepath.Join(c.deps.DataDir, stateFileName)
	st := stateFile{BaseURL: loc.baseURL, Command: loc.command, Args: loc.args, Workdir: loc.workdir}
	if err := core.WriteJSONAtomic(path, st); err != nil {
		c.deps.Log("cannot write %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// Small shared state helpers
// ---------------------------------------------------------------------------

func (c *Client) httpClient() *http.Client {
	if c.deps.HTTPClient != nil {
		return c.deps.HTTPClient
	}
	if c.vendorClient != nil {
		return c.vendorClient
	}
	return defaultHTTPClient
}

var defaultHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	},
}

func (c *Client) cachedCatalog() ([]core.Model, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.catalog) == 0 {
		return nil, false
	}
	if ttl := c.cfg.Timeouts.modelsCacheTTL(); ttl > 0 && time.Since(c.catalogAt) > ttl {
		return nil, false
	}
	out := make([]core.Model, len(c.catalog))
	copy(out, c.catalog)
	return out, true
}

// lastCatalog returns the most recent catalogue regardless of its age.  It is
// what RefreshModels hands back when a refresh fails: a stale-but-real list is
// strictly better than the built-in one.
func (c *Client) lastCatalog() []core.Model {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.catalog) == 0 {
		return nil
	}
	out := make([]core.Model, len(c.catalog))
	copy(out, c.catalog)
	return out
}

// ModelMaxOutputTokens implements core.ModelLimitsProvider.  When the caller
// omits max_tokens the gateway asks what the vendor advertises for this model.
//
// tabbit merges internal/modelmeta into its catalogue on every transport -- the
// web route and the built-in fallback table fill gaps directly, and parseModels
// does the same for the sidecar list -- so a model the embedded table sources
// answers here even when the vendor's own payload omitted the number.  It reads
// the cache without a fetch, and a cold cache answers "cannot say".
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	return core.OutputLimitFor(c.lastCatalog(), model)
}

func (c *Client) storeCatalog(models []core.Model, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.catalog = append([]core.Model(nil), models...)
	c.catalogAt = at
}

func (c *Client) storeHealth(h healthState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health = h
}

func (c *Client) setDiscovered(baseURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.discovered = baseURL
}

// noteError records the last upstream failure and logs it once per distinct
// message.  Error strings never contain credentials (see proxy.go).
func (c *Client) noteError(msg string) {
	msg = truncate(msg, 300)
	c.mu.Lock()
	changed := msg != c.lastErr
	c.lastErr = msg
	c.mu.Unlock()
	if changed {
		c.deps.Log("upstream error: %s", msg)
	}
}

func (c *Client) statusModels() []string {
	if models, ok := c.cachedCatalog(); ok {
		return ids(models)
	}
	return ids(fallbackModels(c.cfg))
}

func ids(models []core.Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if s := strings.TrimSpace(m.ID); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Generic helpers shared by the other files
// ---------------------------------------------------------------------------

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func readLimited(r io.Reader, n int64) []byte {
	if r == nil || n <= 0 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(r, n))
	return b
}

func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := strings.TrimSpace(asString(m[k])); s != "" {
			return s
		}
	}
	return ""
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n, true
		}
	}
	return 0, false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func versionLabel(h healthState) string {
	if strings.TrimSpace(h.version) == "" {
		return "version unknown"
	}
	return "version " + h.version
}

func modelCountLabel(h healthState) string {
	if h.models <= 0 {
		return "model count unknown"
	}
	return fmt.Sprintf("%d models", h.models)
}

func hostSuffix(h healthState, configured string) string {
	host := firstNonEmpty(h.host, configured)
	if host == "" {
		return ""
	}
	return ", web host " + host
}
