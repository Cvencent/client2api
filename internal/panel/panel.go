// Package panel serves the built-in web dashboard.  It is vendor-neutral: it
// only ever reads core.Status values and the OPTIONAL core capabilities, so a
// client module never has to know the panel exists — and the panel never has
// to know which modules exist.
package panel

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"client2api/internal/alerts"
	"client2api/internal/core"
	"client2api/internal/gateway"
	"client2api/internal/livecfg"
	"client2api/internal/modelmeta"
	"client2api/internal/modelpark"
	"client2api/internal/modelpolicy"
	"client2api/internal/scheduler"
)

// Scheduler is the panel's view of the automation timetable.  It is almost
// entirely read-only: a schedule is a plan, and the manual trigger under
// /panel/api/clients/<name>/batches/<batch>/run works whether or not the
// timetable is on.  It is an interface so the panel can be built without a
// scheduler at all.
//
// The one exception is the balance refresh, which is not a batch and therefore
// has nowhere else to go: the reference's POST /panel/api/balance_all called
// Scheduler.RunBalanceRefreshNow, and so does this port.
type Scheduler interface {
	Status() scheduler.Status
	// Config reports the live timetable, so the tasks centre can render
	// each platform's effective hours without reading the config file back.
	Config() scheduler.Config
	// History reports finished batch runs, newest first.  Manual presses
	// and scheduled fires both land here, so one table answers "what ran".
	History() []scheduler.RunRecord
	// RunBalanceRefreshNow refreshes every account's balance off-schedule and
	// reports whether the host had wired a balance hook at all.  False is the
	// "not implemented" answer, so the route answers 501 rather than 200 for
	// work that never happened.
	RunBalanceRefreshNow(ctx context.Context) bool
	// RunRecoveryNow runs one platform's recovery probe off-schedule.
	// False means the host did not wire the hook, so the route must answer
	// 501 instead of pretending the probe happened.
	RunRecoveryNow(ctx context.Context, client string) bool
	// RunDailyBalanceNow runs one platform's early-morning balance sweep
	// off-schedule.  Same 501 contract as RunRecoveryNow: false means the
	// host did not wire the hook, so nothing actually ran.
	RunDailyBalanceNow(ctx context.Context, client string) bool
}

// SessionPlatformStore is the gateway-level half of sticky-session management.
// The panel only needs to list routes and forget one; the concrete gateway
// implementation shares the same table the request path uses.
type SessionPlatformStore interface {
	List() []gateway.StickyRoute
	Forget(conversationKey string) bool
	Pin(conversationKey, platform, model string) bool
}

//go:embed index.html
var indexHTML []byte

//go:embed vendors/three.min.js
var threeJS []byte

//go:embed vendors/motion.min.js
var motionJS []byte

// Options configures the panel.
type Options struct {
	Registry *core.Registry
	Version  string
	Listen   string
	Started  time.Time
	// Reload is called by POST /panel/api/reload.  May be nil.
	Reload func() error
	// Restart is called by POST /panel/api/restart: the host replaces this
	// process with a fresh one so a change the live reload cannot apply (the
	// listen address, the data directory, which clients are enabled, a module's
	// own settings) is one click instead of a trip to a terminal.  May be nil,
	// and the panel then reports the restart as unavailable rather than
	// pretending it happened.
	Restart func() error
	// ConfigPath is the config file the process was actually started with.
	// Empty means "unknown", and /panel/api/config then reports no config
	// rather than guessing a path.
	ConfigPath string
	// DataDir is where the modules keep their own files: the credential
	// stores, the pool health they persist, and the caches.  Empty means
	// "unknown", and /panel/api/bundle then refuses rather than guesses
	// where a pool lives.
	DataDir string
	// AuthEnabled reports whether inbound auth is on.  The key itself is
	// deliberately not part of Options: the panel has no way to leak what it
	// was never given.
	//
	// When Live is set it supersedes this flag, so rotating the key from the
	// config page takes effect on the very next request instead of the next
	// restart.
	AuthEnabled bool
	// Live, when non-nil, carries the settings the panel may change while the
	// process runs.  The panel reads the API key from it (to authenticate
	// management calls) and writes back the fields the config page owns.
	// ModelOverrides is the operator's persisted model metadata. It is shared
	// with the gateway so edits made in the panel take effect on the next
	// request without a restart.
	ModelOverrides *modelmeta.OverrideStore
	// ModelPolicies is the operator's per-account model blacklist, shared with
	// the gateway.  Nil leaves the model dialog read-only except for modules
	// that implement core.AccountModelPolicyManager themselves.
	ModelPolicies *modelpolicy.Store
	// ModelParks is the gateway's model-scoped rate-limit memory, shared so
	// the model dialog can show a 429 even for modules that publish no
	// per-model parks of their own.  Nil disables the merge.
	ModelParks *modelpark.Store
	Live       *livecfg.Holder
	// Guard exposes the process-level egress-IP and degraded-prompt state.
	Guard *core.Guard
	// Stats, Usage and Logs are the gateway's instrumentation, shared by
	// reference so the panel renders the very numbers the gateway serves.
	// All three may be nil, in which case the corresponding endpoint reports
	// an empty payload instead of failing.
	Stats *gateway.Stats
	Usage *gateway.UsageStore
	Logs  *gateway.LogRing
	// Alerts is the persistent platform-health notification journal.  The
	// gateway appends to it and the panel reads it; nil hides the bell count.
	Alerts *alerts.Store
	// Scheduler reports the automation timetable so the dashboard can show
	// that the unattended work exists and when it next fires.  May be nil.
	Scheduler Scheduler
	// Probes is the gateway's context-probe memory, shared by reference so the
	// models table can show which (platform, model) pairs have settled their
	// window and let an operator clear one to re-probe.  Nil hides the column.
	Probes ProbeSource
	// ProbeFile is the model output-limit probe results written out-of-band by
	// probe_max_tokens.py --panel-out.  Empty (or a file that does not exist)
	// makes /panel/api/model_probes answer an empty set, and the models view
	// then shows the vendor's claimed value unannotated.
	ProbeFile string
	// ExpiringSoon is the operator's "expiring soon" window, passed to a
	// BalanceProvider so it can say how much credit will lapse inside it.  Zero
	// means the dashboard asked for no such bucket, and a module must then not
	// invent one -- the reference derives this from its scheduler, we derive it
	// from pool.expiring_soon, because that is where the operator set it.
	ExpiringSoon time.Duration
	// Balances is the process-wide last-known balance cache the accounts page
	// renders. A host that also runs scheduled sweeps builds it with
	// NewBalanceStore and passes the same store to those sweeps, so their
	// successful reads move the column and its "last read" stamp too. Nil
	// makes the panel create a private cache, which is what tests and
	// embedders without a scheduler get.
	Balances *BalanceStore
	// Checkins is the process-wide daily check-in state shared with the
	// scheduler.  Nil makes the panel create a private persistent store from
	// DataDir, which is what tests and embedders without a scheduler want.
	Checkins *CheckinStore
	// PackageDetailLimit is how many credit batches the credits view shows per
	// account before collapsing the rest behind a toggle.  It is resolved
	// against its default by the caller, so the panel only reports it; a
	// non-positive value here means "the caller did not resolve it" and the
	// page falls back to its own copy of the default.
	PackageDetailLimit int
	// SessionPlatforms, when non-nil, lets the panel inspect and clear the
	// gateway's conversation-to-platform bindings alongside each module's
	// account bindings.
	SessionPlatforms SessionPlatformStore
	// InFlight, when non-nil, is the gateway's per-account in-flight counter.
	// The account pool's 在途 column reads it for every module that does not
	// report an in-flight count of its own, and the platform configuration
	// page's per-account ceiling comes from the registry either way.  Nil
	// leaves those cells to whatever the module itself reported.
	InFlight AccountInFlightSource
}

// AccountInFlightSource is the slice of the gateway's in-flight bookkeeping
// the panel needs: how many requests the gateway is carrying against one
// account of one platform right now.  It is an interface rather than the
// concrete store so tests and embedders can stand in for the gateway.
type AccountInFlightSource interface {
	AccountInFlight(platform, accountID string) int
}

// clientView is a module's self-report plus what the panel is allowed to ask
// of it.  The capability block is what lets the frontend render an add form
// for one module and a bare table for another, without any per-client code.
type clientView struct {
	core.Status
	Capabilities core.Capabilities `json:"capabilities"`
	// Pool is present only for a module that keeps an account pool, so the page
	// can tell "this client has no pool" from "this client's pool is idle".
	Pool *core.PoolStats `json:"pool,omitempty"`
	// Health is the same answer /v1/status gives under client_health, so one
	// question does not need two endpoints.
	Health *core.Health `json:"health,omitempty"`
}

// redisModeNoop is the only mode this build has: state lives in memory (plus
// the usage file), and there is no Redis mirror.  The field is reported rather
// than omitted because the page asks the question — "is this process sharing
// state with anything?" — and "no store" is the honest answer, not silence.
const redisModeNoop = "noop"

type snapshot struct {
	Version   string       `json:"version"`
	Listen    string       `json:"listen"`
	Uptime    string       `json:"uptime"`
	StartedAt string       `json:"started_at"`
	Clients   []clientView `json:"clients"`
	RedisMode string       `json:"redis_mode"`
	// Schedule is absent when no scheduler is wired, so a caller can tell
	// "automation is off" from "this build has none".
	Schedule *scheduler.Status `json:"schedule,omitempty"`
}

// New returns an http.Handler serving /panel/ and /panel/api/*.
//
// Every /panel/api/* route is behind the same bearer as the OpenAI surface.
// The HTML shell itself stays open, exactly as in the reference: a browser has
// to be able to load the page before it can be asked for a key.  The page
// prompts for the key and keeps it in the browser, so the server never has to
// treat the shell as trusted.
func New(opts Options) http.Handler {
	mux := http.NewServeMux()
	p := &panel{opts: opts, now: time.Now, runs: newTaskRuns(), sweeps: newBatchRuns(), chores: newTaskQueues(), taskLocks: newAccountLocks()}
	p.initBalanceCache()
	p.initCheckins()
	p.initTaskBoardCache()
	p.initChatHistory()
	p.queue = newBatchQueue(p.sweeps)
	p.queue.run = p.execSweep
	p.queue.report = p.panicReport()

	// api registers a management route behind the shared bearer.
	api := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, p.authed(h))
	}

	api("/panel/api/status", p.handleStatus)
	api("/panel/api/reload", p.handleReload)
	api("/panel/api/restart", p.handleRestart)
	api("/panel/api/clients", p.handleClientList)
	// Everything below is dispatched by hand so that one module growing a new
	// capability never changes the routing table.
	api("/panel/api/clients/", p.handleClientScoped)

	// The polling/aggregate surface the dashboard reads.  These are additive:
	// the routes above keep their exact old behaviour.
	api("/panel/api/overview", p.handleOverview)
	api("/panel/api/logs", p.handleLogs)
	api("/panel/api/models", p.handleModelsList)
	api("/panel/api/models/refresh", p.handleModelsRefresh)
	api("/panel/api/exposed_models", p.handleExposedModels)
	api("/panel/api/model_context", p.handleModelContext)
	api("/panel/api/model_probe/clear", p.handleModelProbeClear)
	api("/panel/api/usage", p.handleUsage)
	api("/panel/api/usage/save", p.handleUsageSave)
	api("/panel/api/chat/conversations", p.handleChatConversations)
	api("/panel/api/chat/conversations/", p.handleChatConversationRoute)
	api("/panel/api/sticky-sessions", p.handleStickySessions)
	api("/panel/api/sticky-sessions/unbind", p.handleStickySessionUnbind)
	api("/panel/api/sticky-sessions/switch", p.handleStickySessionSwitch)
	api("/panel/api/sticky-sessions/switch-all", p.handleStickySessionSwitchAll)
	api("/panel/api/alerts", p.handleAlerts)
	api("/panel/api/config", p.handleConfig)
	api("/panel/api/sources/", p.handleSource)
	// The whole-instance snapshot: the platform routing policy plus every
	// account credential, so a pool can be moved between machines.  GET
	// exports, POST restores.
	api("/panel/api/bundle", p.handleBundle)
	// The tasks centre's automation surface: the live timetable, one row
	// per (platform, batch), and the run journal.  Always 200 (even with no
	// scheduler wired), because the page renders the config either way.
	api("/panel/api/schedule", p.handleSchedule)

	// A GET-only read of a file the gateway never writes.  The method is checked
	// inside the handler, not by a "GET " mux pattern: the "/panel/api/"
	// catch-all below also matches this path, so a method-scoped pattern would
	// let a POST fall through to the catch-all and come back as a bare
	// "404 page not found" instead of the 405 every other route answers.
	api("/panel/api/model_probes", p.handleModelProbes)
	api("/panel/api/route_plan", p.handleRoutePlan)
	api("/panel/api/route_speed", p.handleRouteSpeed)
	// The routing view's live feed: the requests the store still remembers,
	// each with the chain of candidates the router tried for it.
	api("/panel/api/route_live", p.handleRouteLive)

	// The captcha document.  It is a page, not JSON, and it is framed by the
	// shell -- so it gets its own headers (captcha.go) rather than the shell's
	// DENY/frame-ancestors 'none' pair.  Registered as an exact path so the
	// "/panel/" catch-all below keeps owning everything else.
	mux.HandleFunc("/panel/captcha", p.captchaPage)

	// Vendored third-party JS (Three.js, Motion), served from the binary so the
	// panel keeps working fully offline.  Exact allowlist -- the catch-all below
	// must stay the only thing that answers for anything else under /panel/.
	// The bytes never change within a build, so they carry a one-year immutable
	// cache; a new release ships new bytes, and the shell that references them
	// is itself revalidated by ETag, so stale pairs self-heal on the next load.
	gzipVendorOnce()
	mux.HandleFunc("/panel/vendors/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/panel/vendors/")
		var body, gz []byte
		switch name {
		case "three.min.js":
			body, gz = threeJS, threeGZ
		case "motion.min.js":
			body, gz = motionJS, motionGZ
		default:
			http.NotFound(w, r)
			return
		}
		setSecurityHeaders(w)
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		if acceptsGzip(r) && len(gz) > 0 {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Vary", "Accept-Encoding")
			w.Header().Set("Content-Length", strconv.Itoa(len(gz)))
			_, _ = w.Write(gz)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	})

	// The shell carries an ETag of its content hash: a reload asks again with
	// If-None-Match and gets a bodyless 304 instead of 1.2 MB (issue #14).
	// no-store is gone in favour of no-cache semantics -- the page is never
	// used from cache without revalidating, so staleness stays impossible.
	shellETag := fmt.Sprintf(`"%x"`, sha256.Sum256(indexHTML))
	mux.HandleFunc("/panel/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/panel/api/") {
			http.NotFound(w, r)
			return
		}
		// The shell is unauthenticated (a browser has to load the page before it
		// can be asked for a key), so the response headers are the only thing
		// standing between it and a framing/sniffing attack.
		setSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", shellETag)
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, shellETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeGzip(w, r, indexHTML)
	})

	// "/" is the catch-all mounted by the gateway: send browsers to the panel
	// and everything else to a 404.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/panel/", http.StatusFound)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// ProbeSource is the slice of the gateway's context-probe store the panel needs.
//
// It is an interface rather than the concrete store because that type is
// unexported: the gateway owns the storage, and the panel only reads a snapshot
// and asks for one entry to be forgotten.
type ProbeSource interface {
	Snapshot() map[string]map[string]gateway.ProbeRecord
	Clear(client, model string) bool
}

type panel struct {
	opts Options
	// now is the clock the route-speed probe reads.  New installs time.Now so
	// production measures the wall clock; a test may replace it with a
	// deterministic sequence and assert the tok/s denominator without depending
	// on where the wall clock happens to fall inside a timer tick.  Nil falls
	// back to time.Now, which keeps a panel built as a bare struct working.
	now func() time.Time
	// balances is the last-known balance of every account plus the gentle
	// refresh that moves the column forward a few accounts at a time.
	// Created in New; nil when the panel has no registry (some tests).
	balanceCache *balanceCache
	// checkins is today's definitive check-in state for every account.  It is
	// shared with the scheduler when one is wired.
	checkins *CheckinStore
	// taskBoardCache is the last task list seen for each account.  The board
	// reads it before asking a vendor, so revisiting the page only fetches
	// accounts that have never been seen.
	taskBoardCache *taskBoardCache
	// runs journals asynchronous task-board runs.  Created in New so a panel
	// built in a test never sees a nil store.
	runs *taskRuns
	// queue serialises scheduled sweeps.  Two clicks on two buttons must not
	// race each other into the same upstream: the vendors' pacing assumes one
	// sweep at a time, so the second one waits instead of being refused.
	queue *batchQueue
	// sweeps journals those sweeps.  Separate from runs because a sweep is a
	// fan-out (accounts x chores) and a task run is a single chore; sharing one
	// record type would mean either lying about the shape or widening it with
	// fields half the callers ignore.
	sweeps *batchRuns
	// chores drains the fleet-wide task queues (POST tasks/run_queue).  Kept
	// apart from queue above: that one serialises sweeps and must stay a single
	// slot, while these are lists several workers pull from.  One queue per
	// module, so a backlog on one vendor cannot sit in front of another's.
	chores *taskQueues
	// taskLocks serialises the per-account task verbs.  One account must not
	// run two chore actions at once: a vendor chore is stateful (accept, then
	// run, then claim), so two overlapping rounds fight over the same progress
	// and both then report nonsense.  Per account, not per client: the
	// reference keeps the same lock, and two accounts of one vendor are
	// genuinely independent.
	taskLocks *accountLocks
	// chatHistory is the persisted conversation record for the chat test page.
	// It is created in New from DataDir so records follow the data directory
	// instead of living in one browser's localStorage.
	chatHistory *chatHistoryStore
}

func (p *panel) ctx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// clock is the probe's time source.  A panel assembled as a bare struct in a
// test (or by an embedder that never called New) has no clock installed, and
// then reads the wall clock so the probe still measures something real.
func (p *panel) clock() func() time.Time {
	if p == nil || p.now == nil {
		return time.Now
	}
	return p.now
}

// initBalanceCache wires the last-known-balance store.  Split out of New so a
// test can build the same panel and seed the cache before the first request.
func (p *panel) initBalanceCache() {
	if p == nil {
		return
	}
	// A host that also runs scheduled balance sweeps passes the store those
	// sweeps publish into. Adopting it is what keeps the column, its "last
	// read" stamp and the module pool on one reading.
	if p.opts.Balances != nil {
		p.balanceCache = p.opts.Balances.cache
		return
	}
	if p.opts.Registry == nil {
		return
	}
	cachePath := ""
	if p.opts.DataDir != "" {
		cachePath = filepath.Join(p.opts.DataDir, filepath.FromSlash(DefaultBalanceCacheFileName))
	}
	p.balanceCache = newBalanceCache(p.opts.Registry, p.expiringSoon, cachePath)
}

// initTaskBoardCache wires the last-known task lists used by the board.  A
// panel with no data directory still gets an in-memory cache, which keeps
// tests and embedded callers from accidentally re-scanning on every render.
func (p *panel) initTaskBoardCache() {
	if p == nil {
		return
	}
	cachePath := ""
	if p.opts.DataDir != "" {
		cachePath = filepath.Join(p.opts.DataDir, filepath.FromSlash(DefaultTaskBoardCacheFileName))
	}
	p.taskBoardCache = newTaskBoardCache(cachePath)
}

// balanceHandler builds a panel usable from a test: same wiring as New, but
// the *panel stays addressable so a case can seed the balance cache.
func balanceHandler(opts Options) (*panel, http.Handler) {
	mux := http.NewServeMux()
	p := &panel{opts: opts, runs: newTaskRuns(), sweeps: newBatchRuns(), chores: newTaskQueues(), taskLocks: newAccountLocks()}
	p.initBalanceCache()
	p.initCheckins()
	p.initTaskBoardCache()
	p.initChatHistory()
	p.queue = newBatchQueue(p.sweeps)
	p.queue.run = p.execSweep
	p.queue.report = p.panicReport()
	api := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, p.authed(h)) }
	api("/panel/api/clients/", p.handleClientScoped)
	return p, mux
}

// seedBalanceCache stores an already-known balance without asking the vendor,
// so tests can model "this number was read on an earlier visit" -- the state
// the /balances read is designed to serve.
func seedBalanceCache(p *panel, client, id string, bal core.Balance) {
	p.balanceCache.put(client, id, bal)
}

// ---------------------------------------------------------------------------
// Read-only surface
// ---------------------------------------------------------------------------

func (p *panel) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()

	snap := snapshot{
		Version:   p.opts.Version,
		Listen:    p.opts.Listen,
		Uptime:    time.Since(p.opts.Started).Round(time.Second).String(),
		StartedAt: p.opts.Started.Format(time.RFC3339),
		Clients:   []clientView{},
		RedisMode: redisModeNoop,
	}
	if p.opts.Registry != nil {
		for _, c := range p.opts.Registry.All() {
			view := clientView{
				Status:       core.RedactStatus(c.Status(ctx)),
				Capabilities: p.capabilities(ctx, c),
			}
			mergeModelParks(&view.Status, c.Name(), p.opts.ModelParks)
			p.decorateAccountInFlight(c.Name(), view.Status.Accounts)
			// Both are optional capabilities, so a module that has neither is
			// simply reported as such instead of as zeroes.
			if ps, ok := core.PoolStatsOf(c); ok {
				view.Pool = &ps
			}
			if h, ok := core.HealthOf(c); ok {
				view.Health = &h
			}
			snap.Clients = append(snap.Clients, view)
		}
	}
	if p.opts.Scheduler != nil {
		st := p.opts.Scheduler.Status()
		snap.Schedule = &st
	}
	writeJSON(w, http.StatusOK, snap)
}

func (p *panel) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if p.opts.Reload == nil {
		writeErr(w, http.StatusNotImplemented, "reload not wired")
		return
	}
	if err := p.opts.Reload(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
}

// handleRestart replaces the running process with a fresh one.  It answers
// before the host tears the process down, so the panel can tell the operator
// "restarting" instead of having the connection die mid-request.
func (p *panel) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if p.opts.Restart == nil {
		writeErr(w, http.StatusNotImplemented,
			"this process was started without a restart hook; restart it from whatever supervises it")
		return
	}
	if err := p.opts.Restart(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarting": true})
}

func (p *panel) handleClientList(w http.ResponseWriter, r *http.Request) {
	names := []string{}
	if p.opts.Registry != nil {
		names = p.opts.Registry.Names()
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": names, "registered": core.Registered()})
}

// ---------------------------------------------------------------------------
// Per-client dispatch
//
//	GET    <base>/capabilities
//	GET    <base>/accounts
//	POST   <base>/accounts                 add
//	POST   <base>/accounts/refresh         {id?}
//	DELETE <base>/accounts/<id>
//	POST   <base>/accounts/<id>/test
//	POST   <base>/accounts/<id>/enabled    {enabled}
//	POST   <base>/accounts/<id>/model-policy  {disabled_models[],force_enabled}
//	GET    <base>/discover
//	POST   <base>/import                   {paths?, all?}
//	POST   <base>/login
//	GET    <base>/login/<session>
//	DELETE <base>/login/<session>
//
//	GET    <base>/sms                     SMS platform status
//	POST   <base>/sms/phone               rent one number
//	POST   <base>/sms/code                poll once for the code
//	POST   <base>/sms/release             release or blacklist a number
//
//	POST   <base>/auto-login              run the whole login in a browser
//	GET    <base>/auto-login/<id>         read one run's progress
//	DELETE <base>/auto-login/<id>         stop a run
//
//	POST   <base>/phone-code/start      text a code to the operator's own phone
//	POST   <base>/phone-code/finish     redeem that code and store the account
//
//	POST   <base>/accounts/<id>/checkin   {"action":"…","captcha_param":"…","captcha_region":"…"} (all optional)
//
//	GET    <base>/captcha                 ?action=<id> (optional)
//
//	GET    <base>/tasks                   ?account=<id> (optional)
//	POST   <base>/tasks/<code>/run        {"account":"…"} (optional)
//	GET    <base>/tasks/runs/<id>
//
//	GET    <base>/conversations           ?key=…&model=…
//	POST   <base>/conversations           {"account":"…","key":"…"}
//	POST   <base>/conversations/unbind    {"key":"…"}
//
//	POST   <base>/quick-connect/<id>/probe
//	POST   <base>/quick-connect/<id>/connect  {"fields":{"api_key":"…"}}
// ---------------------------------------------------------------------------

func (p *panel) handleClientScoped(w http.ResponseWriter, r *http.Request) {
	// Work from the *escaped* path.  r.URL.Path is already percent-decoded by
	// net/http, so an account id that contains "/" — tabbit's sidecar account id
	// is literally `http://127.0.0.1:50124` — would split into extra segments and
	// 404 before the id was ever reassembled.  Splitting the escaped form first
	// and unescaping each segment afterwards keeps such an id in one piece.
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/panel/api/clients/")
	if rest == "" {
		p.handleClientList(w, r)
		return
	}
	name, tail, _ := strings.Cut(rest, "/")
	if dec, err := url.PathUnescape(name); err == nil {
		name = dec
	}

	if p.opts.Registry == nil {
		writeErr(w, http.StatusServiceUnavailable, "no registry")
		return
	}
	client, ok := p.opts.Registry.Get(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such client: "+name)
		return
	}

	// Split the tail into segments, unescaping each so an account id may
	// contain characters that need escaping in a URL.
	segs := []string{}
	for _, s := range strings.Split(tail, "/") {
		if s == "" {
			continue
		}
		if dec, err := url.PathUnescape(s); err == nil {
			s = dec
		}
		segs = append(segs, s)
	}

	switch {
	case len(segs) == 1 && segs[0] == "capabilities":
		ctx, cancel := p.ctx(r, 10*time.Second)
		defer cancel()
		writeJSON(w, http.StatusOK, p.capabilities(ctx, client))

	// One-click companion-service connections.  Probe is read-only and may
	// be called repeatedly before anything is created; Connect is the only
	// verb that writes.  A module without the capability answers 501.
	case len(segs) == 3 && segs[0] == "quick-connect" && segs[2] == "probe":
		p.quickConnectProbe(w, r, client, segs[1])

	case len(segs) == 3 && segs[0] == "quick-connect" && segs[2] == "connect":
		p.quickConnectConnect(w, r, client, segs[1])

	case len(segs) == 1 && segs[0] == "accounts":
		p.accounts(w, r, client)

	case len(segs) == 2 && segs[0] == "accounts" && segs[1] == "refresh":
		p.refreshAccounts(w, r, client)

	case len(segs) == 2 && segs[0] == "accounts":
		p.accountByID(w, r, client, segs[1])

	case len(segs) == 3 && segs[0] == "accounts" && segs[2] == "test":
		p.testAccount(w, r, client, segs[1])

	case len(segs) == 3 && segs[0] == "accounts" && segs[2] == "enabled":
		p.setEnabled(w, r, client, segs[1])

	case len(segs) == 3 && segs[0] == "accounts" && segs[2] == "model-policy":
		p.setAccountModelPolicy(w, r, client, segs[1])

	case len(segs) == 3 && segs[0] == "accounts" && segs[2] == "checkin":
		p.checkin(w, r, client, segs[1])

	case len(segs) == 1 && segs[0] == "invites":
		p.invites(w, r, client)

	case len(segs) == 2 && segs[0] == "invites" && segs[1] == "bind":
		p.bindInvites(w, r, client)

	case len(segs) == 3 && segs[0] == "accounts" && segs[2] == "redeem":
		p.redeemAccountCode(w, r, client, segs[1])

	// The operator's escape hatch.  Every module's pool parks a credential on
	// its own authority (a dead session, a plan limit, a WAF block), and none
	// of them may shorten that verdict just because time passed.  Only a human
	// can say the vendor side is fixed, so this is the one route that clears
	// every runtime penalty at once.  A module with no penalty state answers
	// 501 rather than showing a button that cannot do anything.
	case len(segs) == 3 && segs[0] == "accounts" && segs[2] == "revive":
		p.reviveAccount(w, r, client, segs[1])

	// What the account still has.  Read-only for us, but POST because it costs
	// a vendor call: a GET is something a browser or a proxy may repeat on its
	// own, and this must only happen when the operator asked.
	case len(segs) == 3 && segs[0] == "accounts" && segs[2] == "balance":
		p.accountBalance(w, r, client, segs[1])

	// The same numbers for every account at once, which is what the accounts
	// page renders in its 余额 column.  A module without BalanceProvider answers
	// 501 and the column is hidden, the usual "no button for an unimplemented
	// mechanism" rule.
	case len(segs) == 1 && segs[0] == "balances":
		p.balances(w, r, client)

	// The explicit "refresh the column now" action behind the toolbar button.
	// It answers from the cache immediately and moves the vendor calls to a
	// paced background pass, so a click never becomes a fleet-wide burst.
	case len(segs) == 2 && segs[0] == "balances" && segs[1] == "refresh":
		p.balancesRefresh(w, r, client)

	case len(segs) == 1 && segs[0] == "packages":
		p.packages(w, r, client)

	// What this module would need from a real browser before the panel can run
	// one of its actions.  The panel asks this only after the operator clicks,
	// then runs the vendor's captcha SDK in the captcha page and hands the
	// result back on the checkin call below.  A module without the capability
	// answers 501 and the panel skips the browser step entirely.
	case len(segs) == 1 && segs[0] == "captcha":
		p.captchaScene(w, r, client)

	case len(segs) == 2 && segs[0] == "school" && segs[1] == "vouchers":
		p.schoolVouchers(w, r, client)

	// Conversation stickiness.  The chat tab needs it to aim a request at one
	// chosen account: ChatRequest carries no account, so the binding table is
	// the only honest lever.  A module without one answers 501.
	case len(segs) == 1 && segs[0] == "conversations":
		p.conversations(w, r, client)

	case len(segs) == 2 && segs[0] == "conversations" && segs[1] == "unbind":
		p.unbindConversation(w, r, client)

	case len(segs) == 1 && segs[0] == "discover":
		p.discover(w, r, client)

	case len(segs) == 1 && segs[0] == "import":
		p.importCreds(w, r, client)

	// An operator-supplied export document.  The panel never parses it -- the
	// module owns the format -- so this route is only about getting the bytes
	// there and reporting the verdict.  A module without BundleImporter answers
	// 501, exactly like every other optional capability.
	case len(segs) == 2 && segs[0] == "import" && segs[1] == "bundle":
		p.importBundle(w, r, client)

	case len(segs) == 1 && segs[0] == "login":
		p.startLogin(w, r, client)

	// The realms an account can be added to.  A module serving one upstream
	// family reports an empty list, and the panel then shows no picker -- the
	// same "no button for an unimplemented mechanism" rule as everywhere else.
	// Matched before the session route below, which would otherwise swallow it.
	case len(segs) == 2 && segs[0] == "login" && segs[1] == "regions":
		p.loginRegions(w, r, client)

	case len(segs) == 2 && segs[0] == "login":
		p.loginBySession(w, r, client, segs[1])

	// The one-time-SMS platform, for adding an account without the operator
	// owning a phone.  A module that never opted in answers 501 from the
	// handler, exactly like every other optional capability.
	case len(segs) == 1 && segs[0] == "sms":
		p.smsStatus(w, r, client)

	case len(segs) == 2 && segs[0] == "sms" && segs[1] == "phone":
		p.smsPhone(w, r, client)

	case len(segs) == 2 && segs[0] == "sms" && segs[1] == "code":
		p.smsCode(w, r, client)

	case len(segs) == 2 && segs[0] == "sms" && segs[1] == "release":
		p.smsRelease(w, r, client)

	// The one-click version: the module drives the vendor's page itself.  A
	// module that never opted in answers 501 from the handler.
	case len(segs) == 1 && segs[0] == "auto-login":
		p.autoLoginStart(w, r, client)

	case len(segs) == 2 && segs[0] == "auto-login":
		p.autoLoginByID(w, r, client, segs[1])

	// The operator-driven phone-code login: the module texts a code to a
	// number the operator owns and they type it back.  A module that never
	// opted in answers 501 from the handler.
	case len(segs) == 2 && segs[0] == "phone-code" && segs[1] == "start":
		p.phoneCodeStart(w, r, client)

	case len(segs) == 2 && segs[0] == "phone-code" && segs[1] == "finish":
		p.phoneCodeFinish(w, r, client)

	case len(segs) == 1 && segs[0] == "tasks":
		p.taskList(w, r, client)

	// The fleet-wide half of the task centre.  "scan_all" reports what every
	// account still owes, "run_queue" works through exactly that list, and
	// "queue" is what the caller polls while it does.
	case len(segs) == 2 && segs[0] == "tasks" && segs[1] == "scan_all":
		p.taskScanAll(w, r, client)

	case len(segs) == 2 && segs[0] == "tasks" && segs[1] == "run_queue":
		p.taskRunQueue(w, r, client)

	case len(segs) == 2 && segs[0] == "tasks" && segs[1] == "queue":
		p.taskQueueStatus(w, r, client)

	case len(segs) == 2 && segs[0] == "tasks" && segs[1] == "runs":
		writeErr(w, http.StatusBadRequest, "missing run id")

	case len(segs) == 3 && segs[0] == "tasks" && segs[1] == "runs":
		p.taskRunStatus(w, r, client, segs[2])

	case len(segs) == 3 && segs[0] == "tasks" && segs[2] == "run":
		p.taskRun(w, r, client, segs[1])

	// The per-account task verbs.  The reference nests them under the account
	// they act on and so do we: acting on one account's board should not be a
	// query-string edit away from acting on another's.
	case len(segs) == 3 && segs[0] == "accounts" && segs[2] == "tasks":
		p.accountTasks(w, r, client, segs[1])

	case len(segs) == 4 && segs[0] == "accounts" && segs[2] == "tasks" && segs[3] == "accept":
		p.taskAccept(w, r, client, segs[1])

	case len(segs) == 4 && segs[0] == "accounts" && segs[2] == "tasks" && segs[3] == "accept_all":
		p.taskAcceptAll(w, r, client, segs[1])

	case len(segs) == 4 && segs[0] == "accounts" && segs[2] == "tasks" && segs[3] == "claim":
		p.taskClaim(w, r, client, segs[1])

	case len(segs) == 4 && segs[0] == "accounts" && segs[2] == "tasks" && segs[3] == "auto":
		p.taskAuto(w, r, client, segs[1])

	case len(segs) == 4 && segs[0] == "accounts" && segs[2] == "tasks" && segs[3] == "auto_all":
		p.taskAutoAll(w, r, client, segs[1])

	// Scheduled sweeps.  A module that declares no batches (core.BatchPlanner)
	// answers 501 here, exactly like every other optional capability: the
	// route exists for the whole fleet, the capability decides who serves it.
	case len(segs) == 1 && segs[0] == "batches":
		p.batchList(w, r, client)

	case len(segs) == 2 && segs[0] == "batches" && segs[1] == "queue":
		p.batchQueueList(w, r, client)

	case len(segs) == 3 && segs[0] == "batches" && segs[1] == "runs":
		p.batchRunStatus(w, r, client, segs[2])

	case len(segs) == 3 && segs[0] == "batches" && segs[2] == "run":
		p.batchStart(w, r, client, segs[1])

	// The reference's one-click verbs, kept verbatim so an operator's habits
	// (and any bookmark) survive the port.  They are aliases, not a second
	// implementation -- except balance_all, which has no batch behind it and is
	// served by the scheduler's fleet-wide refresh instead (see balanceAll).
	case len(segs) == 1 && allVerb(segs[0]):
		name, _ := batchNameFromAllVerb(segs[0])
		if name == balanceVerb {
			p.balanceAll(w, r)
		} else {
			p.batchStart(w, r, client, name)
		}

	default:
		writeErr(w, http.StatusNotFound, "no such panel endpoint: "+r.URL.Path)
	}
}

func (p *panel) accounts(w http.ResponseWriter, r *http.Request, c core.Client) {
	am, ok := core.AsAccountManager(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" does not manage accounts")
		return
	}
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := p.ctx(r, 30*time.Second)
		defer cancel()
		list, err := am.Accounts(ctx)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		p.decorateAccountPriorities(c.Name(), list)
		p.decorateAccountNotes(c.Name(), list)
		p.decorateAccountCheckins(c.Name(), list)
		p.decorateAccountModelPolicies(c.Name(), list)
		out := map[string]any{
			"client":       c.Name(),
			"accounts":     redactAccounts(list),
			"capabilities": p.capabilities(ctx, c),
		}
		// Only present when the gateway has actually seen traffic for some
		// account: an empty object would make the table render zeroes for
		// credentials it has never routed, which reads as "broken".
		if st := p.accountStats(); len(st) > 0 {
			out["stats"] = st
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		var spec core.AccountSpec
		if !decodeJSON(w, r, &spec) {
			return
		}
		ctx, cancel := p.ctx(r, 60*time.Second)
		defer cancel()
		rec, err := am.AddAccount(ctx, spec)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"account":  redactAccount(rec),
			"accounts": p.relist(ctx, am),
		})

	default:
		writeErr(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

func (p *panel) accountByID(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	am, ok := core.AsAccountManager(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" does not manage accounts")
		return
	}
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "use DELETE")
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()
	if err := am.RemoveAccount(ctx, id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":  id,
		"accounts": p.relist(ctx, am),
	})
}

func (p *panel) testAccount(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	am, ok := core.AsAccountManager(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" does not manage accounts")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	_ = decodeBody(r, &body) // optional

	ctx, cancel := p.ctx(r, 120*time.Second)
	defer cancel()
	// body.Model 是操作员在面板上选的模型。只有实现了 ModelTester 的模块才
	// 认这个参数；其余模块（探测固定模型、或根本不发对话）退回原来的行为，
	// 而不是把「你选的模型」悄悄换成它自己的默认模型后仍然报成功。
	want := strings.TrimSpace(body.Model)
	var res core.TestResult
	var err error
	if mt, ok := c.(core.ModelTester); ok && want != "" {
		res, err = mt.TestAccountModel(ctx, id, want)
	} else {
		res, err = am.TestAccount(ctx, id)
	}
	if err != nil {
		// A probe that could not even be attempted (no such account, no
		// model to try) is an error; a probe that ran and failed is a 200
		// with ok=false, because that is a normal outcome.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res.AccountID = id
	res.Error = core.Redact(res.Error)
	res.Reply = core.Redact(res.Reply)
	writeJSON(w, http.StatusOK, map[string]any{
		"result":   res,
		"accounts": p.relist(ctx, am),
	})
}

func (p *panel) setEnabled(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	am, ok := core.AsAccountManager(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" does not manage accounts")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "'enabled' is required")
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()
	if err := am.SetAccountEnabled(ctx, id, *body.Enabled); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": p.relist(ctx, am)})
}

func (p *panel) checkin(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	cp, ok := core.AsCheckinProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no daily check-in")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Action string `json:"action"`
		// CaptchaParam is a token the vendor's own captcha SDK minted in the
		// operator's browser (see captcha.go and core.WithCaptchaSolution).
		// It is optional in both directions: a module that needs none ignores
		// it, and a module that needs one reports the gap itself, because the
		// panel cannot know which actions are gated.
		CaptchaParam  string `json:"captcha_param"`
		CaptchaRegion string `json:"captcha_region"`
	}
	_ = decodeBody(r, &body) // optional; empty selects the module default

	ctx, cancel := p.ctx(r, 120*time.Second)
	defer cancel()
	// A blank param leaves the context untouched, so "the operator sent an
	// empty field" and "the panel did not run a captcha" stay the same thing.
	ctx = core.WithCaptchaSolution(ctx, core.CaptchaSolution{Param: body.CaptchaParam, Region: body.CaptchaRegion})
	res, err := cp.Checkin(ctx, id, strings.TrimSpace(body.Action))
	if err != nil {
		// Could not even be attempted (no such account, no store).  A
		// refusal by the vendor is a 200 with ok=false instead.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if res.AccountID == "" {
		res.AccountID = id
	}
	res.Error = core.Redact(res.Error)
	res.Message = core.Redact(res.Message)
	for k, v := range res.Data {
		if s, isStr := v.(string); isStr {
			res.Data[k] = core.Redact(s)
		}
	}
	p.recordCheckinResult(c.Name(), res.AccountID, strings.TrimSpace(body.Action), res)
	out := map[string]any{"result": res}
	if am, ok := core.AsAccountManager(c); ok {
		out["accounts"] = p.relist(ctx, am)
	}
	writeJSON(w, http.StatusOK, out)
}

func (p *panel) refreshAccounts(w http.ResponseWriter, r *http.Request, c core.Client) {
	am, ok := core.AsAccountManager(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" does not manage accounts")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	_ = decodeBody(r, &body)

	ctx, cancel := p.ctx(r, 180*time.Second)
	defer cancel()
	results, err := am.RefreshAccount(ctx, body.ID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	for i := range results {
		results[i].Error = core.Redact(results[i].Error)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results":  results,
		"accounts": p.relist(ctx, am),
	})
}

func (p *panel) discover(w http.ResponseWriter, r *http.Request, c core.Client) {
	ci, ok := core.AsCredentialImporter(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot import credentials")
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()
	found, err := ci.Discover(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if found == nil {
		found = []core.DiscoveredCredential{}
	}
	for i := range found {
		found[i].Note = core.Redact(found[i].Note)
		found[i].Label = core.Redact(found[i].Label)
	}
	writeJSON(w, http.StatusOK, map[string]any{"detected": found})
}

func (p *panel) importCreds(w http.ResponseWriter, r *http.Request, c core.Client) {
	ci, ok := core.AsCredentialImporter(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot import credentials")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Paths []string `json:"paths"`
		All   bool     `json:"all"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Paths) == 0 && !body.All {
		writeErr(w, http.StatusBadRequest, "pass 'paths' or \"all\": true")
		return
	}
	ctx, cancel := p.ctx(r, 120*time.Second)
	defer cancel()
	imported, err := ci.Import(ctx, body.Paths, body.All)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if imported == nil {
		imported = []core.AccountRecord{}
	}
	res := map[string]any{"imported": redactAccounts(imported)}
	if am, ok := core.AsAccountManager(c); ok {
		res["accounts"] = p.relist(ctx, am)
	}
	writeJSON(w, http.StatusOK, res)
}

// maxBundleBytes bounds one uploaded export.  A credential dump is small -- a
// few hundred accounts is a few hundred kilobytes -- so anything past this is a
// mistake or an attack, and refusing it before it is parsed costs nothing.
const maxBundleBytes = 8 << 20

// importBundle hands an operator-supplied export document to the module that
// owns its format.
//
// The panel deliberately does not parse it: the shape belongs to whichever
// third-party tool produced the dump, and teaching the shared layer that shape
// would make it one more thing every other module had to be compared against.
func (p *panel) importBundle(w http.ResponseWriter, r *http.Request, c core.Client) {
	bi, ok := core.AsBundleImporter(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot import a credential bundle")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	name, data, err := readBundleUpload(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "the uploaded document is empty")
		return
	}
	ctx, cancel := p.ctx(r, 120*time.Second)
	defer cancel()
	rep, err := bi.ImportBundle(ctx, name, data)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// The module promises not to echo secrets, but the report is rendered in a
	// browser on a loopback origin, so it is redacted again here rather than
	// trusted.
	errs := make([]string, 0, len(rep.Errors))
	for _, e := range rep.Errors {
		errs = append(errs, core.Redact(e))
	}
	res := map[string]any{
		"ok":       true,
		"total":    rep.Total,
		"imported": rep.Imported,
		"skipped":  rep.Skipped,
		"errors":   errs,
	}
	if am, ok := core.AsAccountManager(c); ok {
		res["accounts"] = p.relist(ctx, am)
	}
	writeJSON(w, http.StatusOK, res)
}

// readBundleUpload pulls the document out of either accepted upload shape.
//
// The reference implementation's browser form posts multipart/form-data with a
// "file" field, and that is the shape the shipped page uses.  A raw body is
// also accepted because a scripted caller gains nothing from assembling a
// multipart envelope by hand, and both shapes carry exactly one document.
//
// It is a free function rather than a method because it touches no panel state.
func readBundleUpload(r *http.Request) (string, []byte, error) {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.HasPrefix(ct, "multipart/form-data") {
		// The limit is enforced on the read below rather than only here:
		// ParseMultipartForm spills past maxMemory to a temp file, so it bounds
		// the in-memory copy, not what the caller may send.
		if err := r.ParseMultipartForm(maxBundleBytes); err != nil {
			return "", nil, fmt.Errorf("parse form: %w", err)
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			return "", nil, fmt.Errorf("missing file field: %w", err)
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, maxBundleBytes))
		if err != nil {
			return "", nil, fmt.Errorf("read file: %w", err)
		}
		name := "upload"
		if hdr != nil {
			if fn := strings.TrimSpace(hdr.Filename); fn != "" {
				name = filepath.Base(fn)
			}
		}
		return name, data, nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBundleBytes))
	if err != nil {
		return "", nil, fmt.Errorf("read body: %w", err)
	}
	return "body", data, nil
}

func (p *panel) startLogin(w http.ResponseWriter, r *http.Request, c core.Client) {
	lp, ok := core.AsLoginProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no interactive login")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	// The body is optional, so a single-realm module is started exactly as
	// before.  A body we cannot read is refused rather than silently
	// defaulted: an account added to the wrong realm is a credential that can
	// never serve the models the operator wanted.
	body, err := loginRequestBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := p.ctx(r, 60*time.Second)
	defer cancel()
	var st core.LoginState
	if pp, ok := core.AsProxyLoginProvider(c); ok && body.ProxyGiven {
		st, err = pp.StartLoginWithProxy(ctx, body.Proxy)
		if err == nil && strings.TrimSpace(p.opts.ConfigPath) != "" {
			_, _, serr := writeConfigKeys(p.opts.ConfigPath, map[string]any{
				"clients": map[string]any{c.Name(): map[string]any{"proxy": body.Proxy}},
			})
			if serr != nil {
				cancel()
				writeErr(w, http.StatusInternalServerError, "login started, but saving clients."+c.Name()+".proxy failed: "+serr.Error())
				return
			}
		}
	} else if rp, ok := core.AsRealmLoginProvider(c); ok && body.Realm != "" {
		st, err = rp.StartLoginRealm(ctx, body.Realm)
	} else {
		st, err = lp.StartLogin(ctx)
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	st.Message = core.Redact(st.Message)
	writeJSON(w, http.StatusOK, st)
}

// loginRealmBody reads the optional {"realm":"..."} body of a login start.  An
// absent or empty body means "the module's configured default", which is what
// every caller before the realm picker sent.
type loginStartBody struct {
	Realm      string `json:"realm"`
	Proxy      string `json:"proxy"`
	ProxyGiven bool   `json:"-"`
}

func loginRequestBody(r *http.Request) (loginStartBody, error) {
	if r.Body == nil {
		return loginStartBody{}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<12))
	if err != nil {
		return loginStartBody{}, errors.New("the request body could not be read")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return loginStartBody{}, nil
	}
	var fields map[string]json.RawMessage
	if uerr := json.Unmarshal(raw, &fields); uerr != nil {
		return loginStartBody{}, errors.New(`the request body must be JSON: {"realm":"cn"|"global","proxy":"http://127.0.0.1:7890"}`)
	}
	var body loginStartBody
	if uerr := json.Unmarshal(raw, &body); uerr != nil {
		return loginStartBody{}, errors.New(`the request body must be JSON: {"realm":"cn"|"global","proxy":"http://127.0.0.1:7890"}`)
	}
	_, body.ProxyGiven = fields["proxy"]
	body.Realm = strings.TrimSpace(body.Realm)
	body.Proxy = strings.TrimSpace(body.Proxy)
	return body, nil
}

// loginRealmBody is kept as the narrow helper used by callers that only need
// the realm.  It has the same optional-body semantics as loginRequestBody.
func loginRealmBody(r *http.Request) (string, error) {
	body, err := loginRequestBody(r)
	return body.Realm, err
}

// loginRegions answers which realms an account can be added to for one module.
// An empty list is the honest answer for a single-realm module, and the panel
// then renders no picker at all -- the same rule as every other optional
// mechanism: no button for something that cannot be done.
func (p *panel) loginRegions(w http.ResponseWriter, r *http.Request, c core.Client) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	realms := []core.LoginRealm{}
	if rp, ok := core.AsRealmLoginProvider(c); ok {
		ctx, cancel := p.ctx(r, 10*time.Second)
		defer cancel()
		realms = rp.LoginRealms(ctx)
	}
	writeJSON(w, http.StatusOK, map[string]any{"client": c.Name(), "realms": realms})
}

func (p *panel) loginBySession(w http.ResponseWriter, r *http.Request, c core.Client, session string) {
	lp, ok := core.AsLoginProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no interactive login")
		return
	}
	ctx, cancel := p.ctx(r, 60*time.Second)
	defer cancel()

	switch r.Method {
	case http.MethodGet:
		st, err := lp.PollLogin(ctx, session)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		st.Message = core.Redact(st.Message)
		writeJSON(w, http.StatusOK, st)
	case http.MethodDelete:
		if err := lp.CancelLogin(ctx, session); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"state": core.LoginCancelled})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "use GET or DELETE")
	}
}

// relist re-reads the account list after a mutation so every response carries
// fresh state and the frontend never has to guess.
func (p *panel) relist(ctx context.Context, am core.AccountManager) []core.AccountRecord {
	list, _ := p.relistErr(ctx, am)
	return list
}

// relistErr is relist plus the error, for callers that have somewhere to put
// it.  Both always return a non-nil slice so the JSON carries [] and not null.
func (p *panel) relistErr(ctx context.Context, am core.AccountManager) ([]core.AccountRecord, error) {
	list, err := am.Accounts(ctx)
	if err != nil {
		return []core.AccountRecord{}, err
	}
	p.decorateAccountPriorities(am.Name(), list)
	p.decorateAccountNotes(am.Name(), list)
	p.decorateAccountCheckins(am.Name(), list)
	p.decorateAccountModelPolicies(am.Name(), list)
	return redactAccounts(list), nil
}

// capabilities is core.CapabilitiesOf plus the one flag only the deployment
// can answer: whether the panel holds a store for a per-account model
// blacklist when the module does not own one itself.  A module that implements
// core.AccountModelPolicyManager keeps its own -- WorkBuddy does, because its
// model ids carry a realm -- so the fallback flag is deliberately not set for
// it.
func (p *panel) capabilities(ctx context.Context, c core.Client) core.Capabilities {
	caps := core.CapabilitiesOf(ctx, c)
	if p != nil && p.opts.ModelPolicies != nil && caps.Manage && !caps.ModelPolicy {
		caps.ModelPolicyStore = true
	}
	return caps
}

// decorateAccountModelPolicies fills in the operator's per-account blacklist
// for a module whose policy lives in the panel's shared store rather than in
// the module's own file.  A record that already carries disabled_models owns
// the field -- the module listed it itself -- and is left untouched.
func (p *panel) decorateAccountModelPolicies(platform string, list []core.AccountRecord) {
	if p == nil || p.opts.ModelPolicies == nil {
		return
	}
	for i := range list {
		if _, owned := list[i].Fields["disabled_models"]; owned {
			continue
		}
		disabled := p.opts.ModelPolicies.Disabled(platform, list[i].ID)
		if len(disabled) == 0 {
			continue
		}
		if list[i].Fields == nil {
			list[i].Fields = map[string]any{}
		}
		// A string slice, not []any: redactAccounts only walks the latter, so
		// this keeps the operator's model ids verbatim while everything else
		// still goes through redaction.
		list[i].Fields["disabled_models"] = append([]string(nil), disabled...)
	}
}

func (p *panel) decorateAccountPriorities(platform string, list []core.AccountRecord) {
	if p == nil || p.opts.Registry == nil {
		return
	}
	for i := range list {
		list[i].Priority = p.opts.Registry.AccountPriority(platform, list[i].ID)
	}
}

// decorateAccountNotes attaches the operator's own label for each account --
// the phone number or e-mail they recorded so a re-login can sign back in as
// the right identity.  It is display metadata: the module's own Label is left
// untouched and the frontend decides which of the two to show.
func (p *panel) decorateAccountNotes(platform string, list []core.AccountRecord) {
	if p == nil || p.opts.Registry == nil {
		return
	}
	for i := range list {
		if note := strings.TrimSpace(p.opts.Registry.AccountNote(platform, list[i].ID)); note != "" {
			list[i].OperatorNote = note
		}
	}
}

// decorateAccountInFlight fills the account pool's 在途 column from the two
// sources that actually know the answer:
//
//   - the ceiling is the operator's per-account setting on the platform
//     configuration page (max_in_flight_per_account, with the per-source
//     override for relay platforms); the gateway enforces exactly that value,
//     so it is the honest "最大并发数".  A module that enforces a stricter
//     ceiling of its own (workbuddy's 号池单账号在途, say) wins when it is
//     lower, because that is the number the account really stops at.
//   - the current count is the module's own report when it keeps one, and the
//     gateway's per-account slot counter otherwise.  The gateway counts every
//     request it routes to the account, including streams, so "the module says
//     nothing" no longer has to mean "the cell says —".
//
// A ceiling of 0 (both sides unlimited) is written as 0 and the page renders
// ∞, which keeps the one display rule the 在途 column ever had.
func (p *panel) decorateAccountInFlight(platform string, accounts []core.AccountStatus) {
	if p == nil || len(accounts) == 0 {
		return
	}
	ceiling := 0
	if p.opts.Registry != nil {
		ceiling = p.opts.Registry.MaxInFlightPerAccountFor(platform)
	}
	for i := range accounts {
		extra := accounts[i].Extra
		if extra == nil {
			extra = map[string]any{}
			accounts[i].Extra = extra
		}
		if _, own := extra["in_flight"]; !own && p.opts.InFlight != nil {
			extra["in_flight"] = p.opts.InFlight.AccountInFlight(platform, accounts[i].ID)
		}
		if p.opts.Registry == nil {
			continue
		}
		limit := ceiling
		if own := ceilingOf(extra["in_flight_limit"]); own > 0 && (limit == 0 || own < limit) {
			limit = own
		}
		extra["in_flight_limit"] = limit
	}
}

// ceilingOf reads a module-reported in_flight_limit.  Modules write plain ints
// into Extra; the wider switch only keeps the decoration honest if one ever
// writes a JSON-ish number type instead.
func ceilingOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func redactAccounts(in []core.AccountRecord) []core.AccountRecord {
	if in == nil {
		return []core.AccountRecord{}
	}
	out := make([]core.AccountRecord, len(in))
	for i, a := range in {
		out[i] = redactAccount(a)
	}
	return out
}

func redactAccount(a core.AccountRecord) core.AccountRecord {
	a.ID = core.Redact(a.ID)
	a.Label = core.Redact(a.Label)
	a.Note = core.Redact(a.Note)
	a.ExpiresAt = core.Redact(a.ExpiresAt)
	// Identity is the vendor's account id, not a secret, but it is redacted
	// with everything else so a module that ever puts token material here
	// cannot leak it through this field.
	a.Identity = core.Redact(a.Identity)
	if a.Fields != nil {
		if v, ok := core.RedactAny(a.Fields).(map[string]any); ok {
			a.Fields = v
		}
	}
	for action, st := range a.Checkins {
		st.Message = core.Redact(st.Message)
		a.Checkins[action] = st
	}
	return a
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": core.Redact(msg)})
}

// decodeBody reads and decodes a JSON request body, tolerating an empty body.
func decodeBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// decodeJSON is decodeBody plus the HTTP error response.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeBody(r, v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
