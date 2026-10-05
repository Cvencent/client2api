package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// The workbuddy client module.
//
// WorkBuddy is Tencent's coding agent.  It has two independently routed realms:
// "cn" (copilot.tencent.com / codebuddy.cn) and the international one
// ("intl", served from www.workbuddy.ai and spelled "global" in the reference
// implementation).  An account belongs to exactly one realm, and the realm
// decides which host a request goes to.
//
// Everything in this package is a port of the MIT-licensed reference project
// client2api-lab/_upstream/workbuddy2api-panel, rewired onto core.Deps /
// core.Client / core.Stream.  See README.md for provenance.

func init() {
	core.Register("workbuddy", New)
}

const (
	// accountReloadInterval throttles re-globbing the credential directory.
	accountReloadInterval = 15 * time.Second
	// modelsFetchTimeout bounds a catalogue refresh triggered from Models().
	modelsFetchTimeout = 20 * time.Second
	// modelsTTL is how long a fetched catalogue is considered fresh.  The
	// reference shortened this from an hour to ten minutes (PR #38: a model the
	// backend just added must show up in the panel immediately, and /v1/models
	// may lag at most one TTL).  Going shorter is not worth it -- every expiry
	// costs two upstream probes.
	modelsTTL = 10 * time.Minute
	// modelsFailCooldown is the negative cache: after a failed catalogue fetch
	// no further probe happens for this long.  Without it, a client that polls
	// /v1/models through an outage turns that outage into a flood.  A catalogue
	// failure is deliberately NOT reported to the pool: it says nothing about
	// whether the account can serve chat, and feeding it to the breaker would
	// penalise a healthy chat path for a models-endpoint hiccup.
	modelsFailCooldown = 5 * time.Minute
	// defaultMaxAttempts is how many accounts one Chat may try.
	defaultMaxAttempts = 3
	// defaultRefreshWindow refreshes an access token this long before expiry.
	defaultRefreshWindow = 5 * time.Minute
)

// There is deliberately no built-in model list.  The reference is pure-dynamic
// (internal/server/handler.go: "无静态兜底——拉不出目录即意味着上游不可用，
// 假名单只会让客户端选到 11102 的模型"), and it is right: a hard-coded id the
// backend no longer serves only makes a client pick a model that answers 11102.
// "Cannot read the catalogue" is therefore reported as an empty catalogue, never
// papered over with guesses.  A list the operator wrote in the config file is
// still honoured (configuredModels): that is a human's deliberate choice, not a
// fabricated default.

// config is the schema of the `clients.workbuddy` object in the main config
// file.  Every field is optional: the module works with no configuration at
// all, discovering credentials in its own data directory.
type config struct {
	AccountsDir    string `json:"accounts_dir"`
	ChatBaseCN     string `json:"chat_base_cn"`
	ChatBaseGlobal string `json:"chat_base_global"`
	BillingBaseCN  string `json:"billing_base_cn"`
	// BillingBaseGlobal is the reference's `global.billing_base`: the
	// international realm's billing host, overridable separately from the chat
	// host.  The register wizard follows it too.
	BillingBaseGlobal string `json:"billing_base_global"`
	WebBaseCN         string `json:"web_base_cn"`
	GlobalEnabled     *bool  `json:"global_enabled"`
	ClientVersion     string `json:"client_version"`
	CliVersion        string `json:"cli_version"`
	UserAgent         string `json:"user_agent"`
	ClientName        string `json:"client_name"`
	PassthroughIP     *bool  `json:"passthrough_ip"`
	DeviceToken       string `json:"device_token"`
	DeviceTokenFile   string `json:"device_token_file"`
	// TimeoutSeconds is the reference's `timeout_seconds`: the ceiling on a short
	// RPC (token refresh, check-in, balance, FetchModels).  Absent or
	// non-positive means 120s.
	TimeoutSeconds *int `json:"timeout_seconds"`
	// HeaderTimeoutSeconds is the reference's `header_timeout_seconds`: how long
	// a chat attempt may wait for the first response byte before the transport
	// gives up and the gateway rotates to another account.  Absent or
	// non-positive falls back to TimeoutSeconds, which is the reference's
	// normalize() rule and keeps "swap account before the first byte" tied to the
	// short-RPC ceiling rather than to an unrelated second number.
	HeaderTimeoutSeconds *int `json:"header_timeout_seconds"`
	// IdleTimeoutSeconds is the reference's `idle_timeout_seconds`: how long a
	// running stream may stay silent before it is cancelled.  Absent means the
	// reference's 300s; an explicit 0 is this module's documented escape hatch
	// for "do not monitor the stream at all".
	IdleTimeoutSeconds *int     `json:"idle_timeout_seconds"`
	MaxAttempts        *int     `json:"max_attempts"`
	RefreshWindowSecs  *int     `json:"refresh_window_seconds"`
	Models             []string `json:"models"`

	// LoginRealm selects the realm the panel's "sign in" button authorises
	// against: "cn" (default) or "global" / "intl".
	LoginRealm string `json:"login_realm"`
	// LoginTTLSeconds is how long an authorisation URL stays valid.  Zero or
	// absent means defaultLoginTTL (15 minutes).
	LoginTTLSeconds *int `json:"login_ttl_seconds"`
	// LoginPollSeconds is advisory: the panel drives the cadence, this only
	// documents the expected interval.
	LoginPollSeconds *int `json:"login_poll_seconds"`

	// SMSToken is the one-time-SMS platform credential (eomsg).  Empty means
	// the panel's 接码 controls report "not configured" and refuse to rent a
	// number; the operator can still paste a token into the panel, which
	// overrides this per call without a restart.
	SMSToken string `json:"sms_token"`
	// SMSKeyword is the sender keyword the platform filters on.  Absent means
	// defaultSMSKeyword.
	SMSKeyword string `json:"sms_keyword"`
	// SMSBase overrides the platform endpoint.  Absent means the built-in
	// eomsg URL, which is the only provider this module speaks.
	SMSBase string `json:"sms_base"`
	// SMSProvinces replaces the built-in rotation pool.  An empty list keeps
	// the built-in 31 provinces; "none" disables the rotation entirely and
	// lets the platform choose.
	SMSProvinces []string `json:"sms_provinces"`

	// BrowserPath pins the Chromium-family executable the auto-login flow
	// drives.  Empty means "find an installed Edge or Chrome".
	BrowserPath string `json:"browser_path"`
	// BrowserHeadless runs that browser without a window.  Absent means true.
	// A visible window is easier to debug and less likely to trip a vendor's
	// bot heuristics, but it needs a desktop session to draw on.
	BrowserHeadless *bool `json:"browser_headless"`
	// AutoLoginTimeoutSeconds bounds one auto-login run end to end.  Absent
	// means 300s, which is wb-auto's --timeout default.
	AutoLoginTimeoutSeconds *int `json:"auto_login_timeout_seconds"`
	// SMSPolls is how many times one rented number is checked for its SMS
	// before the run gives up on that number.  Absent means 12.
	SMSPolls *int `json:"sms_polls"`
	// SMSIntervalSeconds is the pause between two SMS polls.  Absent means 5s.
	SMSIntervalSeconds *int `json:"sms_interval_seconds"`
	// DupRetries is how many extra numbers to draw when the platform keeps
	// handing back one that is already in the pool.  Absent means 6.
	DupRetries *int `json:"dup_retries"`

	// SanitizeFingerprints mirrors the reference's
	// `features.sanitize_blacklist_fingerprints` switch, which defaults to TRUE
	// in the original panel: request bodies are scrubbed of the markers that
	// give away which coding agent actually produced them, on the last step of
	// the body pipeline (see sanitize.go).  A pointer so an explicit `false`
	// can turn the scrubber off.
	SanitizeFingerprints *bool `json:"sanitize_fingerprints"`
}

func (c config) maxAttempts() int {
	if c.MaxAttempts != nil && *c.MaxAttempts > 0 {
		return *c.MaxAttempts
	}
	return defaultMaxAttempts
}

func (c config) refreshWindow() time.Duration {
	if c.RefreshWindowSecs != nil && *c.RefreshWindowSecs >= 0 {
		return time.Duration(*c.RefreshWindowSecs) * time.Second
	}
	return defaultRefreshWindow
}

// totalTimeout resolves `timeout_seconds`: the ceiling on a short RPC.  The
// reference's default is two minutes, and it applies to an absent value and to a
// non-positive one alike (a zero-length ceiling would fail every call).
func (c config) totalTimeout() time.Duration {
	if c.TimeoutSeconds != nil && *c.TimeoutSeconds > 0 {
		return time.Duration(*c.TimeoutSeconds) * time.Second
	}
	return defaultHTTPTimeout
}

// headerTimeout resolves `header_timeout_seconds`: the chat first-byte deadline.
// A missing or non-positive value falls back to `timeout_seconds`, which is the
// reference's normalize() rule.
func (c config) headerTimeout() time.Duration {
	if c.HeaderTimeoutSeconds != nil && *c.HeaderTimeoutSeconds > 0 {
		return time.Duration(*c.HeaderTimeoutSeconds) * time.Second
	}
	return c.totalTimeout()
}

// idleTimeout resolves `idle_timeout_seconds`: how long a running stream may stay
// silent.  An absent key means the reference's 300s; an explicit value is
// honoured as written, so a configured 0 still switches the monitor off.
func (c config) idleTimeout() time.Duration {
	if c.IdleTimeoutSeconds != nil {
		return time.Duration(*c.IdleTimeoutSeconds) * time.Second
	}
	return defaultIdleTimeout
}

// sanitizeFingerprints reports whether the body scrubber should run.  Absent
// configuration means on, matching the reference's default.
func (c config) sanitizeFingerprints() bool {
	if c.SanitizeFingerprints != nil {
		return *c.SanitizeFingerprints
	}
	return true
}

// browserHeadless resolves `browser_headless`.  Absent means headless: the
// common case is a host with no desktop to draw on, and a run that wants a
// visible window is the exception an operator opts into.
func (c config) browserHeadless() bool {
	if c.BrowserHeadless != nil {
		return *c.BrowserHeadless
	}
	return true
}

// autoLoginTimeout resolves `auto_login_timeout_seconds`.
func (c config) autoLoginTimeout() time.Duration {
	if c.AutoLoginTimeoutSeconds != nil && *c.AutoLoginTimeoutSeconds > 0 {
		return time.Duration(*c.AutoLoginTimeoutSeconds) * time.Second
	}
	return defaultAutoLoginTimeout
}

// smsPolls resolves `sms_polls`: how many times one number is checked for its
// SMS before the run gives up on it.
func (c config) smsPolls() int {
	if c.SMSPolls != nil && *c.SMSPolls > 0 {
		return *c.SMSPolls
	}
	return defaultSMSPolls
}

// smsInterval resolves `sms_interval_seconds`: the pause between two SMS
// polls.
func (c config) smsInterval() time.Duration {
	if c.SMSIntervalSeconds != nil && *c.SMSIntervalSeconds > 0 {
		return time.Duration(*c.SMSIntervalSeconds) * time.Second
	}
	return defaultSMSInterval
}

// dupRetries resolves `dup_retries`: how many extra numbers to draw when the
// platform keeps handing back one that is already in the pool.
func (c config) dupRetries() int {
	if c.DupRetries != nil && *c.DupRetries > 0 {
		return *c.DupRetries
	}
	return defaultDupRetries
}

// Client implements core.Client.
type Client struct {
	deps core.Deps
	cfg  config
	up   *Upstream
	pool *Pool

	// affinity pins a conversation to the account that already warmed the
	// vendor's prompt cache for it.  See affinity.go: it is created by New, and
	// every method on it is nil-safe, so a Client assembled by a test without
	// New keeps working with stickiness simply switched off.
	affinity *core.Affinity

	accountsMu   sync.Mutex
	accountsAt   time.Time
	accountsNote string

	modelsMu     sync.RWMutex
	models       []core.Model
	modelsAt     time.Time
	modelsFailAt time.Time // negative cache; guarded by modelsMu

	// One-time-SMS platform state.  smsRotation holds the provinces this
	// round has not drawn from yet, so consecutive rents do not land in the
	// same province; smsLastProv is the one just handed out.  Guarded by
	// smsMu, lazily initialised, so a zero-value Client stays usable.
	smsMu       sync.Mutex
	smsRotation []string
	smsLastProv string

	// Browser-login sessions, keyed by the vendor's state token.  Lazily
	// created so New() stays unchanged.
	loginMu sync.Mutex
	logins  map[string]*panelLogin

	// Auto-login jobs, keyed by the id handed to the panel.  Lazily created
	// for the same reason as logins: a zero-value Client stays usable.
	autoMu sync.Mutex
	autos  map[string]*autoJob

	// The system-prompt strategy, which a live reload can retune.  Read on
	// every request, so it is guarded rather than plain.
	promptMu   sync.RWMutex
	promptMode string
	promptText string
}

// New builds the module.  It never returns an error for missing credentials:
// that is a runtime condition reported by Status(), not a construction failure.
func New(deps core.Deps) (core.Client, error) {
	// The chore runners, the board fallbacks and the creditor paths call
	// deps.Logf directly; deps.Log is the nil-safe wrapper, but those sites
	// predate it.  A caller that built core.Deps by hand without a logger
	// would otherwise panic halfway through a chore, so fill the hole once
	// here.  That is also what lets those call sites stay unguarded.
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}
	cfg := config{}
	if len(deps.Config) > 0 {
		if err := json.Unmarshal(deps.Config, &cfg); err != nil {
			deps.Log("workbuddy: invalid config, using defaults: %v", err)
			cfg = config{}
		}
	}
	if deps.DataDir != "" {
		if err := core.EnsureDir(deps.DataDir); err != nil {
			deps.Log("workbuddy: cannot create data dir %s: %v", deps.DataDir, err)
		}
	}

	up := NewUpstream()
	up.Logf = deps.Logf
	up.Sanitize = cfg.sanitizeFingerprints()
	if cfg.ChatBaseCN != "" {
		up.ChatBaseCN = cfg.ChatBaseCN
	}
	if cfg.ChatBaseGlobal != "" {
		up.ChatBaseGlobal = cfg.ChatBaseGlobal
	}
	if cfg.BillingBaseCN != "" {
		up.BillingBaseCN = cfg.BillingBaseCN
	}
	if cfg.BillingBaseGlobal != "" {
		up.BillingBaseGlobal = cfg.BillingBaseGlobal
	}
	if cfg.WebBaseCN != "" {
		up.WebBaseCN = cfg.WebBaseCN
	}
	if cfg.GlobalEnabled != nil {
		up.GlobalEnabled = *cfg.GlobalEnabled
	}
	if cfg.ClientVersion != "" {
		up.ClientVersion = cfg.ClientVersion
	}
	if cfg.CliVersion != "" {
		up.CliVersion = cfg.CliVersion
	}
	if cfg.UserAgent != "" {
		up.UserAgent = cfg.UserAgent
	}
	if cfg.ClientName != "" {
		up.ClientName = cfg.ClientName
	}
	if cfg.PassthroughIP != nil {
		up.PassthroughIP = *cfg.PassthroughIP
	}
	if cfg.DeviceToken != "" {
		up.DeviceToken = cfg.DeviceToken
	}
	if cfg.DeviceTokenFile != "" {
		up.DeviceTokenFile = cfg.DeviceTokenFile
	}
	// The reference's `upstream.*_timeout_seconds` trio, resolved here so the
	// header deadline can fall back to the short-RPC ceiling.  This has to run
	// before the first request: the header deadline is installed on the shared
	// transport.
	up.SetTimeouts(cfg.totalTimeout(), cfg.headerTimeout(), cfg.idleTimeout())
	// A caller-supplied HTTP client (proxy, custom TLS) is honoured for the
	// control-plane calls; the chat path keeps its own no-total-timeout client
	// but inherits the caller's transport when one was supplied.
	if deps.HTTPClient != nil {
		up.HTTP = deps.HTTPClient
		if up.ChatHTTP != nil && deps.HTTPClient.Transport != nil {
			up.ChatHTTP.Transport = deps.HTTPClient.Transport
		}
	}

	c := &Client{deps: deps, cfg: cfg, up: up}
	// Passthrough is the reference's default, and it has to be initialised
	// explicitly: the zero value is "no strategy", which the prompt stage and
	// the Degrade hook both read as "not one of the three".
	c.promptMode = promptModePassthrough
	c.pool = NewPool(nil, cfg.refreshWindow())
	// Restore the cooldowns recorded before the last shutdown *before* the
	// first refresh: the staged records are handed out by Replace, which is
	// where the accounts they describe are actually built.
	c.pool.AttachState(c.poolStateDir(), deps.Log)
	// Conversation→account stickiness.  The 30-minute idle TTL and the 5-minute
	// sweep interval are core's defaults, i.e. the reference's numbers.  The GC
	// goroutine lives for the process, which is the same lifetime as the
	// client: there is no Close on core.Client to hang a stop from.
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()
	c.refreshAccounts(true)
	c.loadModelCache()
	return c, nil
}

// Name implements core.Client.
func (c *Client) Name() string { return "workbuddy" }

// accountsDir is where credential JSON files live.
func (c *Client) accountsDir() string {
	if strings.TrimSpace(c.cfg.AccountsDir) != "" {
		return c.cfg.AccountsDir
	}
	return c.deps.DataDir
}

// refreshAccounts reloads the credential directory, at most every
// accountReloadInterval unless force is set.  It never blocks on the network.
func (c *Client) refreshAccounts(force bool) {
	c.accountsMu.Lock()
	defer c.accountsMu.Unlock()
	if !force && time.Since(c.accountsAt) < accountReloadInterval {
		return
	}
	c.accountsAt = time.Now()
	dir := c.accountsDir()
	if dir == "" {
		c.accountsNote = "no data directory configured"
		c.pool.Replace(nil)
		return
	}
	accounts, err := LoadAccounts(dir)
	if err != nil {
		c.accountsNote = "cannot read credential directory: " + err.Error()
		c.pool.Replace(nil)
		return
	}
	c.pool.Replace(accounts)
	if len(accounts) == 0 {
		c.accountsNote = "no credential files (*.json) in " + dir
		return
	}
	c.accountsNote = ""
}

// --- models ----------------------------------------------------------------

type modelCacheDoc struct {
	FetchedAt time.Time    `json:"fetched_at"`
	Models    []core.Model `json:"models"`
}

func (c *Client) modelCachePath() string {
	if c.deps.DataDir == "" {
		return ""
	}
	return filepath.Join(c.deps.DataDir, "cache", "models.json")
}

// poolStateDir is where the account pool's health file lives.  It is a
// subdirectory of the data directory on purpose: the credential loader globs
// <accounts_dir>/*.json (auth.go), so a state file written beside the
// credentials is enumerated and parsed as one of them.  This module already
// keeps its other non-credential JSON — the model cache — in the same place.
func (c *Client) poolStateDir() string {
	if c.deps.DataDir == "" {
		return ""
	}
	return filepath.Join(c.deps.DataDir, "cache")
}

func (c *Client) loadModelCache() {
	p := c.modelCachePath()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var doc modelCacheDoc
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Models) == 0 {
		return
	}
	c.modelsMu.Lock()
	c.models = doc.Models
	c.modelsAt = doc.FetchedAt
	c.modelsMu.Unlock()
}

func (c *Client) storeModels(models []core.Model, fetchedAt time.Time) {
	c.modelsMu.Lock()
	c.models = models
	c.modelsAt = fetchedAt
	c.modelsMu.Unlock()
	p := c.modelCachePath()
	if p == "" {
		return
	}
	if err := core.EnsureDir(filepath.Dir(p)); err != nil {
		c.up.log("workbuddy: cannot create model cache dir: %v", err)
		return
	}
	if err := core.WriteJSONAtomic(p, modelCacheDoc{FetchedAt: fetchedAt, Models: models}); err != nil {
		c.up.log("workbuddy: cannot persist model cache: %v", err)
	}
}

func (c *Client) cachedModels() ([]core.Model, time.Time) {
	c.modelsMu.RLock()
	defer c.modelsMu.RUnlock()
	out := make([]core.Model, len(c.models))
	copy(out, c.models)
	return out, c.modelsAt
}

func (c *Client) modelIDs() []string {
	models, _ := c.cachedModels()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// ModelMaxOutputTokens implements core.ModelLimitsProvider.  When the caller
// omits max_tokens the gateway asks what the vendor advertises for this model,
// instead of leaving the request without a budget.
//
// It reads the same cached catalogue Models answers from and never fetches: it
// runs inside a chat request, so a stale or empty catalogue has to mean "cannot
// say" rather than a metadata round trip.  The vendor's own list carries
// maxOutputTokens per model and modelextra.go publishes it as
// Extra["max_output_tokens"], which is what core.OutputLimitFor reads.
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	models, _ := c.cachedModels()
	return core.OutputLimitFor(models, model)
}

// configuredModels is the operator's own list from the config file.  It is the
// only non-upstream source of model ids: when the key is absent the catalogue is
// exactly what upstream said, including "nothing".
func (c *Client) configuredModels() []core.Model {
	out := make([]core.Model, 0, len(c.cfg.Models))
	for _, id := range c.cfg.Models {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out = append(out, core.Model{ID: id, OwnedBy: "workbuddy"})
	}
	return out
}

// modelCacheState reads the catalogue, when it was fetched, and when it last
// failed, under one lock so the three can never disagree.
func (c *Client) modelCacheState() (models []core.Model, fetchedAt, failedAt time.Time) {
	c.modelsMu.RLock()
	defer c.modelsMu.RUnlock()
	out := make([]core.Model, len(c.models))
	copy(out, c.models)
	return out, c.modelsAt, c.modelsFailAt
}

// noteModelsFailure arms the negative cache and clears the freshness stamp, so
// a failed probe is not mistaken for a fresh catalogue.
func (c *Client) noteModelsFailure() {
	c.modelsMu.Lock()
	c.modelsFailAt = time.Now()
	c.modelsMu.Unlock()
}

// noteModelsSuccess clears the negative cache: one good answer means the vendor
// is reachable again and the next request may probe immediately.
func (c *Client) noteModelsSuccess() {
	c.modelsMu.Lock()
	c.modelsFailAt = time.Time{}
	c.modelsMu.Unlock()
}

// serveWithoutProbing answers a catalogue request without touching upstream:
// the last good list when there is one, otherwise the operator's configured
// list, otherwise nothing at all.
func (c *Client) serveWithoutProbing(cached []core.Model) []core.Model {
	if len(cached) > 0 {
		return cached
	}
	return c.configuredModels()
}

// Models implements core.Client.  It serves the cached catalogue while it is
// fresh, honours the negative cache, and otherwise asks upstream once.  When
// upstream cannot answer it returns what it already has -- possibly nothing:
// the reference is pure-dynamic, so an empty catalogue is the honest answer, and
// the gateway shows the picker empty rather than offering models that would
// answer 11102.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	c.refreshAccounts(false)

	cached, fetchedAt, failedAt := c.modelCacheState()
	if len(cached) > 0 && !fetchedAt.IsZero() && time.Since(fetchedAt) < modelsTTL {
		return cached, nil
	}
	if !failedAt.IsZero() && time.Since(failedAt) < modelsFailCooldown {
		return c.serveWithoutProbing(cached), nil
	}
	if c.pool.Ready() {
		models, err := c.fetchAllRealmModels()
		if err == nil && len(models) > 0 {
			c.storeModels(models, time.Now())
			c.noteModelsSuccess()
			return models, nil
		}
		if err != nil {
			c.up.log("workbuddy: model catalogue refresh failed: %v", err)
		}
		c.noteModelsFailure()
	}
	return c.serveWithoutProbing(cached), nil
}

// RefreshModels implements core.ModelRefresher.  It bypasses both the
// in-memory modelsTTL window and the on-disk cache and asks the vendor again,
// which is what the panel's "re-fetch from upstream" button needs.
//
// A failed refresh must never empty the catalogue.  Every error path returns
// the last good list alongside the error -- or the operator's configured list
// when nothing has ever been fetched -- so one flaky refresh cannot blank the
// model picker.  Unlike Models it ignores the negative cache: an operator who
// presses the button gets a real probe, not a silent no-op.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.refreshAccounts(false)

	previous, _ := c.cachedModels()
	fallback := previous
	if len(fallback) == 0 {
		fallback = c.configuredModels()
	}

	if !c.pool.Ready() {
		return fallback, fmt.Errorf("workbuddy: refresh models: %w", core.ErrNotConfigured)
	}
	models, err := c.fetchAllRealmModels()
	if err != nil {
		c.up.log("workbuddy: model catalogue refresh failed: %v", err)
		c.noteModelsFailure()
		return fallback, fmt.Errorf("workbuddy: refresh models: %s", c.scrubAll(describeFailure(err)))
	}
	c.storeModels(models, time.Now())
	c.noteModelsSuccess()
	return models, nil
}

// scrubSecret renders an upstream failure without ever echoing the credentials
// we sent.  core.Redact catches credential-shaped text (bearer headers, JWTs,
// "token=..." labels); the account's own tokens are replaced verbatim as well,
// because a bare token carries no label for core.Redact to key on.
func scrubSecret(msg string, secrets ...string) string {
	msg = core.Redact(msg)
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, core.MaskSecret(s))
		}
	}
	return msg
}

// scrubAll is scrubSecret against every credential the pool currently holds.  A
// catalogue refresh fans out over several accounts at once -- one per realm -- so
// there is no single account whose tokens to redact.  Redacting against all of
// them is strictly safer than picking one and hoping it was the one that failed.
func (c *Client) scrubAll(msg string) string {
	var secrets []string
	for _, a := range c.pool.Accounts() {
		if a == nil {
			continue
		}
		secrets = append(secrets, a.AccessTokenValue(), a.RefreshTokenValue())
	}
	return scrubSecret(msg, secrets...)
}

// fetchModelsBounded runs the (blocking) catalogue fetch under a deadline.  The
// fetch itself cannot be cancelled mid-flight, so the goroutine is allowed to
// finish and its result is discarded on timeout.
//
// realm is the realm the account belongs to and is stamped onto every id it
// returns.  The vendor's own ids are bare, so without the stamp a dual-realm
// install would publish "glm-5.2" twice with no way to tell the two apart -- and
// the picker would then be free to route either one to either realm.
func (c *Client) fetchModelsBounded(a *Auth, realm string) ([]core.Model, error) {
	type result struct {
		infos []ModelInfo
		err   error
	}
	ch := make(chan result, 1)
	core.GoSafe("workbuddy model catalogue fetch", func(msg string) {
		// The caller blocks in the select below until either this channel or the
		// timer fires.  A panicking fetch would otherwise leave the channel
		// empty and cost the caller the whole modelsFetchTimeout, with the
		// reason replaced by a timeout it did not actually hit.
		ch <- result{err: errors.New(msg)}
	}, func() {
		infos, err := c.up.FetchModels(a)
		ch <- result{infos: infos, err: err}
	})
	timer := time.NewTimer(modelsFetchTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		if realm == "" {
			realm = realmCN
		}
		out := make([]core.Model, 0, len(r.infos))
		for _, mi := range r.infos {
			if mi.ID == "" {
				continue
			}
			m := core.Model{ID: qualifyModelID(realm, mi.ID), OwnedBy: "workbuddy"}
			extra := modelExtra(mi)
			if extra == nil {
				extra = map[string]any{}
			}
			// The realm is stated in the id as well.  It is repeated here
			// because the panel renders the extra map, and a table cell that has
			// to re-parse an id to find the realm is one refactor away from
			// rendering it wrong.
			extra["realm"] = realm
			m.Extra = extra
			out = append(out, m)
		}
		if len(out) == 0 {
			return nil, errors.New("catalogue returned no models")
		}
		return out, nil
	case <-timer.C:
		return nil, errors.New("model catalogue fetch timed out")
	}
}

// fetchAllRealmModels asks every realm the pool holds an account for, in
// parallel, and returns the union with realm-qualified ids.
//
// It replaces the old single Pick(nil) probe, which asked whichever account the
// rotation happened to hand out and therefore published exactly one realm's
// catalogue: on a dual-realm install the domestic models were listed and the
// international ones were invisible, so a perfectly good international account
// looked like it had no models.
//
// A realm that fails does not fail the refresh -- the catalogue is the union of
// what answered, which is the honest answer and keeps one broken realm from
// blanking the picker.  Only when nothing at all answered is the error returned.
func (c *Client) fetchAllRealmModels() ([]core.Model, error) {
	realms := c.pool.Realms()
	if len(realms) == 0 {
		return nil, fmt.Errorf("workbuddy: no account to read a catalogue from: %w", core.ErrNotConfigured)
	}

	type result struct {
		models []core.Model
		err    error
	}
	results := make([]result, len(realms))
	// One completion token per realm, sent as the last statement of whichever
	// path ran.  A WaitGroup would publish results[i] to the collector below
	// before core.GoSafe's report had written it, because the report runs after
	// fn's defers; the channel send is a real happens-before edge instead.
	done := make(chan struct{}, len(realms))
	for i, realm := range realms {
		core.GoSafe("workbuddy realm catalogue", func(msg string) {
			results[i] = result{err: errors.New(msg)}
			done <- struct{}{}
		}, func() {
			a, ok := c.pool.PickForModelInRealm(nil, "", realm)
			if !ok {
				results[i] = result{err: fmt.Errorf("no usable %s account", realm)}
				done <- struct{}{}
				return
			}
			models, err := c.fetchModelsBounded(a, realm)
			results[i] = result{models: models, err: err}
			done <- struct{}{}
		})
	}
	for range realms {
		<-done
	}

	out := make([]core.Model, 0, 128)
	var firstErr error
	for i, r := range results {
		if r.err != nil {
			c.up.log("workbuddy: %s model catalogue unavailable: %v", realms[i], r.err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s catalogue: %w", realms[i], r.err)
			}
			continue
		}
		out = append(out, r.models...)
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, errors.New("catalogue returned no models")
	}
	return out, nil
}

// --- chat ------------------------------------------------------------------

// Chat implements core.Client.  It returns core.ErrNotConfigured when no usable
// credential exists, core.ErrUnsupported for a request it cannot express, and a
// classified upstream error otherwise.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, core.ErrUnsupported
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, core.ErrUnsupported
	}
	// Resolve the realm before anything else reads the model name.  The prefix is
	// this gateway's addressing scheme, not something the vendor knows, so it has
	// to come off the wire body while the full name stays available to the picker
	// and to the per-model cooldown bookkeeping (which is keyed by the bare id,
	// because that is what upstream's 11102 and 6004 answers talk about).
	route := routeModel(model)
	if route.bare == "" {
		return nil, core.ErrUnsupported
	}

	c.refreshAccounts(false)
	if c.pool.Len() == 0 {
		return nil, core.ErrNotConfigured
	}

	// The conversation identity is derived from the request as the caller sent
	// it, before anything below rewrites the body: the turn key is a function
	// of the message list, so taking it after the pairing repair or the prompt
	// rewrite would let it drift between steps of the same turn.  The reference
	// draws the same line for the same reason.
	meta := ChatMeta{ConversationID: strings.TrimSpace(req.ConversationID)}
	if meta.ConversationID == "" {
		meta.ConversationID = optionString(req.Options, "conversation_id", "conversationId")
	}
	if meta.ConversationID == "" {
		meta.ConversationID = strings.TrimSpace(req.User)
	}
	// The stickiness key.  It is derived once, before the rotation loop, because
	// it must not change between attempts: a retry on another account has to
	// re-bind the SAME conversation, not start choosing a new one.
	convKey := conversationKey(req, meta)
	// The vendor aggregates its own usage reports by X-Conversation-Request-ID,
	// and the official client resets that id once per user submit.  So one turn
	// must produce exactly one value for every attempt — rotation, retry and
	// downgrade alike — while a new user message must produce a new one.  The
	// caller's own id wins when it sent one.
	meta.ConversationRequestID = conversationRequestID(req.ConversationRequestID, convKey, req)

	// The realm prefix is this gateway's addressing scheme, not something the
	// vendor knows, so it has to come off the wire body.  Pass the bare id
	// instead of copying the request: a shallow copy would carry the gateway's
	// per-account lease, and the slot taken on the copy would never be freed.
	body, err := buildWireBodyFor(req, route.bare)
	if err != nil {
		return nil, err
	}
	// The system-prompt stage runs here, exactly once per request and before
	// the rotation loop.  It cannot live in Upstream.prepareBody, which runs
	// once per attempt: Append is not idempotent, so a retry would stack a
	// second copy of the operator's prompt onto the first.
	body = c.applyPrompt(body, degradeRequested(req))

	skip := map[string]bool{}
	attempts := c.cfg.maxAttempts()
	// Try every account the platform currently holds before declaring the
	// platform itself exhausted.  max_attempts is a floor, not a ceiling:
	// an install with eight accounts must not stop after three just because
	// the historical default was three.
	if n := c.pool.Len(); n > attempts {
		attempts = n
	}
	var lastErr error
	// busy records that the gateway's per-account ceiling (which can be
	// tighter than the pool's own) refused an account we had already taken.
	busy := false
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a, ok := c.pickAccount(skip, convKey, route)
		if !ok {
			break
		}
		// Tell the gateway which credential is about to be used.  On a retry the
		// next iteration overwrites this, so the value the gateway reads once
		// Chat returns is the account of the attempt that actually got through.
		skip[a.ID()] = true
		// Take the in-flight slot before the call and hold it until the
		// response body is closed: that is the window the vendor's risk control
		// measures, so it is the window the ceiling has to cover.  Losing the
		// race for the last slot is not a failure — the account is skipped and
		// the rotation tries the next one.
		if !c.pool.Acquire(a) {
			continue
		}
		// The gateway's per-account ceiling may be tighter than the pool's own.
		// When it refuses, hand the pool slot back and try another account.
		if err := req.AcquireAccountSlot(a.ID()); err != nil {
			c.pool.Release(a)
			busy = true
			continue
		}
		// Tell the gateway which credential is about to be used.  On a retry the
		// next iteration overwrites this, so the value the gateway reads once
		// Chat returns is the account of the attempt that actually got through.
		core.NoteServedBy(req, a.ID())

		rc, err := c.attempt(ctx, a, body, meta)
		if err == nil {
			if rc == nil {
				c.pool.Release(a)
				lastErr = errors.New("upstream returned an empty response body")
				continue
			}
			c.pool.MarkSuccessForModel(a, route.bare)
			return newLeasedStream(ctx, rc, func() { c.pool.Release(a) }), nil
		}
		c.pool.Release(a)
		// MarkFailureForModel rather than MarkFailure: a 11102 or a model-scoped
		// 6004 parks the model on this account, not the account, because the
		// credential is still perfectly good for every other model.
		kind, _ := c.pool.MarkFailureForModel(a, route.bare, err)
		lastErr = err
		if !retryableKind(kind) {
			return nil, err
		}
	}
	if lastErr != nil {
		return nil, errors.Join(core.ErrPlatformExhausted, lastErr)
	}
	// Nothing was attempted because every usable account is at its ceiling:
	// that is backpressure, not a configuration problem, and saying so is what
	// lets a caller back off instead of hunting for a credential that is fine.
	if c.pool.AllFull() {
		return nil, core.ErrBusy
	}
	if busy {
		return nil, core.ErrBusy
	}
	return nil, core.ErrNotConfigured
}

// attempt performs one upstream call for one account, including the
// refresh-and-retry-once behaviour for a dead session.
func (c *Client) attempt(ctx context.Context, a *Auth, body []byte, meta ChatMeta) (io.ReadCloser, error) {
	if w := c.cfg.refreshWindow(); w > 0 && a.NeedsRefresh(w) {
		if err := c.refreshAccount(a); err != nil {
			return nil, err
		}
	}
	rc, _, _, err := c.up.ChatStream(ctx, a, body, "", meta)
	if err == nil {
		return rc, nil
	}
	var ue *Error
	if asError(err, &ue) && ue.Kind == ErrSessionDead {
		if rerr := c.refreshAccount(a); rerr != nil {
			return nil, err
		}
		rc, _, _, err = c.up.ChatStream(ctx, a, body, "", meta)
	}
	return rc, err
}

// refreshAccount refreshes and persists one account's token.
func (c *Client) refreshAccount(a *Auth) error {
	if err := c.up.RefreshToken(a); err != nil {
		c.up.log("workbuddy: token refresh failed for account %s: %v", core.MaskSecret(a.ID()), err)
		c.pool.MarkInvalid(a, "token refresh failed")
		return err
	}
	if err := a.SaveAtomic(); err != nil {
		c.up.log("workbuddy: could not persist refreshed token for %s: %v", core.MaskSecret(a.ID()), err)
	}
	return nil
}

// --- status ----------------------------------------------------------------

// Status implements core.Client.  It is deliberately cheap: no network calls,
// only a (throttled) directory glob.
func (c *Client) Status(ctx context.Context) core.Status {
	c.refreshAccounts(false)
	st := core.Status{
		Name:      "workbuddy",
		UpdatedAt: time.Now(),
		Accounts:  c.pool.Snapshot(),
		Models:    c.modelIDs(),
	}
	if len(st.Models) == 0 {
		// The catalogue is pure-dynamic, so an empty list is a real answer.
		// Only the operator's own configured ids are shown in that case.
		for _, m := range c.configuredModels() {
			st.Models = append(st.Models, m.ID)
		}
	}

	if c.pool.Len() == 0 {
		note := c.accountsNote
		if note == "" {
			note = "no credentials"
		}
		detail := "not configured: " + note
		if vendor := VendorStoreNote(); vendor != "" {
			detail += " (a WorkBuddy/CodeBuddy store exists at " + vendor +
				" but its tokens are encrypted vendor-side; place a plaintext credential JSON in the data directory)"
		}
		st.Ready = false
		st.Detail = detail
		return st
	}
	if !c.pool.Ready() {
		st.Ready = false
		st.Detail = "all accounts unavailable (" + c.pool.Summary() + ")"
		return st
	}
	st.Ready = true
	realms := map[string]int{}
	for _, a := range c.pool.Accounts() {
		realms[a.RealmName()]++
	}
	parts := make([]string, 0, len(realms))
	for r, n := range realms {
		parts = append(parts, fmt.Sprintf("%s=%d", r, n))
	}
	st.Detail = fmt.Sprintf("%s; realms: %s", c.pool.Summary(), strings.Join(parts, ","))
	return st
}

// --- helpers ---------------------------------------------------------------

func asError(err error, target **Error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if errors.As(err, &ue) {
		*target = ue
		return true
	}
	return false
}

func optionString(opts map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := opts[k].(string); ok {
			if t := strings.TrimSpace(s); t != "" {
				return t
			}
		}
	}
	return ""
}

// normalizeFinish maps an upstream finish_reason onto an OpenAI one.  A stream
// that produced tool calls is reported as "tool_calls" even when upstream said
// "stop", because the caller must still execute them.
func normalizeFinish(finish string, sawToolCall bool) string {
	switch f := strings.ToLower(strings.TrimSpace(finish)); f {
	case "length", "content_filter":
		return f
	case "tool_calls", "function_call":
		return "tool_calls"
	case "stop", "":
		if sawToolCall {
			return "tool_calls"
		}
		return "stop"
	default:
		if sawToolCall {
			return "tool_calls"
		}
		return "stop"
	}
}

func usageFromMap(u map[string]any) *core.Usage {
	if u == nil {
		return nil
	}
	get := func(keys ...string) int {
		for _, k := range keys {
			if n, ok := num64(u[k]); ok {
				return int(n)
			}
		}
		return 0
	}
	out := &core.Usage{
		PromptTokens:     get("prompt_tokens", "input_tokens"),
		CompletionTokens: get("completion_tokens", "output_tokens"),
		TotalTokens:      get("total_tokens"),
		ReasoningTokens:  get("reasoning_tokens"),
		CachedTokens:     get("cached_tokens", "prompt_cache_hit_tokens", "cache_read_input_tokens"),
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	if out.PromptTokens == 0 && out.CompletionTokens == 0 && out.TotalTokens == 0 && out.CachedTokens == 0 {
		return nil
	}
	return out
}

// --- stream ----------------------------------------------------------------

var errStopStream = errors.New("stop stream")

// wbStream turns the upstream SSE body into core.Events.  A goroutine reads the
// body and pushes events into a buffered channel; Recv pulls them.  The
// goroutine exits as soon as the context is cancelled, so a consumer that stops
// reading cannot leak it.
type wbStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	body   io.ReadCloser
	ch     chan core.Event
	parser *DSMLParser

	closeOnce sync.Once
	closeErr  error
}

func newWBStream(parent context.Context, body io.ReadCloser) *wbStream {
	ctx, cancel := context.WithCancel(parent)
	s := &wbStream{
		ctx:    ctx,
		cancel: cancel,
		body:   body,
		ch:     make(chan core.Event, 32),
		parser: NewDSMLParser(),
	}
	// GoSafe is the last-resort net: run reports a panic as an event, but a
	// panic in that report path would still take the whole process down.
	core.GoSafe("workbuddy stream", nil, s.run)
	return s
}

// leasedStream ties a pool in-flight slot to a response body: the slot lives
// exactly as long as the body can still be read, and is returned once, on
// Close.  The gateway closes every stream it opens, which is what makes that
// safe to rely on; Close is idempotent, and so is Release, so a double close
// cannot hand the same slot back twice.
type leasedStream struct {
	*wbStream
	once    sync.Once
	release func()
}

// Close implements core.Stream.  The body is closed first and the slot returned
// after, so a caller that immediately opens another request cannot be admitted
// onto a slot the vendor still considers busy.
func (s *leasedStream) Close() error {
	err := s.wbStream.Close()
	s.once.Do(s.release)
	return err
}

func newLeasedStream(parent context.Context, body io.ReadCloser, release func()) *leasedStream {
	return &leasedStream{wbStream: newWBStream(parent, body), release: release}
}

// Recv implements core.Stream.
func (s *wbStream) Recv() (core.Event, error) {
	ev, ok := <-s.ch
	if !ok {
		return core.Event{}, io.EOF
	}
	return ev, nil
}

// Close implements core.Stream and is idempotent.
func (s *wbStream) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.body != nil {
			s.closeErr = s.body.Close()
		}
	})
	return s.closeErr
}

func (s *wbStream) emit(ev core.Event) bool {
	select {
	case s.ch <- ev:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *wbStream) run() {
	defer close(s.ch)
	// Registered last, so it runs first — while s.ch is still open and s.ctx is
	// still alive, which is what lets the panic be reported as an event instead
	// of surfacing to the caller as a stream that simply stopped.
	defer func() {
		if r := recover(); r != nil {
			s.emit(core.Event{
				Type: core.EventError,
				Err:  fmt.Errorf("workbuddy: the stream reader panicked: %v", r),
			})
		}
	}()

	var (
		usage       *core.Usage
		finish      string
		sawToolCall bool
		emittedAny  bool
		seenTool    = map[int]bool{}
		nativeIndex = map[int]int{}
		nextIndex   int
	)

	emitToolCall := func(idx int, id, name, args string) bool {
		sawToolCall = true
		emittedAny = true
		return s.emit(core.Event{
			Type: core.EventToolCall,
			ToolCall: &core.ToolCallDelta{
				Index:     idx,
				ID:        id,
				Name:      name,
				Arguments: args,
			},
		})
	}

	process := func(obj map[string]any) bool {
		if obj == nil {
			return true
		}
		if raw, hasErr := obj["error"]; hasErr {
			msg := "upstream stream error"
			if m, ok := raw.(map[string]any); ok && m != nil {
				if s, ok := m["message"].(string); ok && strings.TrimSpace(s) != "" {
					msg = strings.TrimSpace(s)
				}
			}
			s.emit(core.Event{Type: core.EventError, Err: errors.New(msg)})
			return false
		}
		if u, ok := obj["usage"].(map[string]any); ok && u != nil {
			if parsed := usageFromMap(normalizeUsageCacheAliases(ensureUsageTotal(u))); parsed != nil {
				usage = parsed
			}
		}
		choices, _ := obj["choices"].([]any)
		for _, craw := range choices {
			ch, ok := craw.(map[string]any)
			if !ok {
				continue
			}
			if fr, _ := ch["finish_reason"].(string); strings.TrimSpace(fr) != "" {
				finish = strings.TrimSpace(fr)
			}
			delta, ok := ch["delta"].(map[string]any)
			if !ok {
				continue
			}
			if rc, _ := delta["reasoning_content"].(string); rc != "" {
				emittedAny = true
				if !s.emit(core.Event{Type: core.EventDelta, Reasoning: rc}) {
					return false
				}
			}
			if txt, _ := delta["content"].(string); txt != "" {
				text, calls := s.parser.Feed(txt)
				if text != "" {
					emittedAny = true
					if !s.emit(core.Event{Type: core.EventDelta, Delta: text}) {
						return false
					}
				}
				for _, call := range calls {
					idx := nextIndex
					nextIndex++
					if !emitToolCall(idx, "", call.Name, call.Arguments) {
						return false
					}
				}
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, traw := range tcs {
					tc, ok := traw.(map[string]any)
					if !ok {
						continue
					}
					upstream := 0
					if f, ok := tc["index"].(float64); ok {
						upstream = int(f)
					}
					// Remap the upstream index onto a fresh one so fragments of
					// the same call keep sharing an index while never colliding
					// with the indexes handed to DSML-parsed calls.
					idx, mapped := nativeIndex[upstream]
					if !mapped {
						idx = nextIndex
						nextIndex++
						nativeIndex[upstream] = idx
					}
					id, _ := tc["id"].(string)
					var name, args string
					if fn, ok := tc["function"].(map[string]any); ok && fn != nil {
						name, _ = fn["name"].(string)
						args, _ = fn["arguments"].(string)
					}
					if seenTool[idx] {
						name = ""
					} else {
						seenTool[idx] = true
					}
					if !emitToolCall(idx, id, name, args) {
						return false
					}
				}
			}
		}
		return true
	}

	readErr := ReadSSEFrames(s.body, func(obj map[string]any, done bool) error {
		if done {
			return nil
		}
		if !process(obj) {
			return errStopStream
		}
		return nil
	})

	if !errors.Is(readErr, errStopStream) {
		if text, calls := s.parser.Finish(); text != "" || len(calls) > 0 {
			if text != "" {
				emittedAny = true
				s.emit(core.Event{Type: core.EventDelta, Delta: text})
			}
			for _, call := range calls {
				idx := nextIndex
				nextIndex++
				emitToolCall(idx, "", call.Name, call.Arguments)
			}
		}
	}
	if readErr != nil && !errors.Is(readErr, errStopStream) && !errors.Is(readErr, io.EOF) {
		if IsEmptyStreamError(readErr) && !emittedAny {
			s.emit(core.Event{Type: core.EventError, Err: readErr})
			return
		}
		if !IsEmptyStreamError(readErr) {
			s.emit(core.Event{Type: core.EventError, Err: readErr})
			return
		}
	}
	if s.ctx.Err() != nil {
		return
	}
	if usage != nil {
		s.emit(core.Event{Type: core.EventUsage, Usage: usage})
	}
	s.emit(core.Event{Type: core.EventDone, Finish: normalizeFinish(finish, sawToolCall)})
}
